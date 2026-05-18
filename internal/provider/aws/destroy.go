package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Destroy tears a cluster down in reverse dependency order. Idempotent: each
// step skips resources that are already gone, so re-running after a partial
// failure completes safely.
//
// S3 backups are intentionally NOT touched — they're cluster identity for
// disaster recovery and have their own lifecycle policy on the shared bucket
// (see docs/operator.md).
func (p *Provider) Destroy(ctx context.Context, name, env string) error {
	steps := []struct {
		label string
		fn    func() error
	}{
		{"asg", func() error { return p.destroyASG(ctx, name, env) }},
		{"launch-template", func() error { return p.destroyLaunchTemplate(ctx, name, env) }},
		{"control-plane", func() error { return p.destroyControlPlane(ctx, name, env) }},
		{"control-nlb", func() error { return p.destroyControlNLB(ctx, name, env) }},
		{"control-eip", func() error { return p.releaseControlEIP(ctx, name, env) }},
		{"iam", func() error { return p.destroyIAM(ctx, name, env) }},
		{"security-groups", func() error { return p.destroySecurityGroups(ctx, name, env) }},
		{"route-table", func() error { return p.destroyRouteTable(ctx, name, env) }},
		{"subnet", func() error { return p.destroySubnet(ctx, name, env) }},
		{"igw", func() error { return p.destroyIGW(ctx, name, env) }},
		{"vpc", func() error { return p.destroyVPC(ctx, name, env) }},
		{"parameters", func() error { return p.destroyParameters(ctx, name, env) }},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			return fmt.Errorf("destroy %s: %w", s.label, err)
		}
	}
	return nil
}

func (p *Provider) destroyASG(ctx context.Context, name, env string) error {
	asgName := "bonsai-" + name + "-" + env + "-workers"
	out, err := p.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return err
	}
	if len(out.AutoScalingGroups) == 0 {
		return nil
	}
	if _, err := p.asg.DeleteAutoScalingGroup(ctx, &autoscaling.DeleteAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(asgName),
		ForceDelete:          aws.Bool(true),
	}); err != nil {
		return err
	}
	// Wait for the ASG to actually disappear — security groups can't be deleted
	// while ENIs from terminated instances are still dereferenced.
	return p.waitForASGGone(ctx, asgName)
}

func (p *Provider) waitForASGGone(ctx context.Context, asgName string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := p.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
			AutoScalingGroupNames: []string{asgName},
		})
		if err != nil {
			return err
		}
		if len(out.AutoScalingGroups) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("asg %s did not delete within 10m", asgName)
}

func (p *Provider) destroyLaunchTemplate(ctx context.Context, name, env string) error {
	ltName := "bonsai-" + name + "-" + env + "-worker"
	_, err := p.ec2.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
	})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (p *Provider) destroyControlPlane(ctx context.Context, name, env string) error {
	out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: append(roleFilters(name, env, "control-plane"),
			ec2types.Filter{Name: aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"}}),
	})
	if err != nil {
		return err
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			ids = append(ids, aws.ToString(inst.InstanceId))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
		return err
	}
	return p.waitForInstancesGone(ctx, ids)
}

func (p *Provider) waitForInstancesGone(ctx context.Context, ids []string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
		if err != nil {
			return err
		}
		gone := true
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if inst.State != nil && inst.State.Name != ec2types.InstanceStateNameTerminated {
					gone = false
				}
			}
		}
		if gone {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("instances %v did not terminate within 10m", ids)
}

func (p *Provider) destroyIAM(ctx context.Context, name, env string) error {
	roleName := iamName(name, env)

	// Detach role from instance profile + delete it.
	_, err := p.iam.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
		RoleName:            aws.String(roleName),
	})
	if err != nil && !isIAMNotFound(err) {
		return fmt.Errorf("remove role from profile: %w", err)
	}
	_, err = p.iam.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
	})
	if err != nil && !isIAMNotFound(err) {
		return fmt.Errorf("delete instance profile: %w", err)
	}

	// Delete inline policies + detach managed policies before the role
	// itself — IAM requires the role to have no policy references.
	policies, err := p.iam.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(roleName)})
	if err != nil && !isIAMNotFound(err) {
		return fmt.Errorf("list role policies: %w", err)
	}
	if err == nil {
		for _, pName := range policies.PolicyNames {
			if _, err := p.iam.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
				RoleName: aws.String(roleName), PolicyName: aws.String(pName),
			}); err != nil && !isIAMNotFound(err) {
				return fmt.Errorf("delete policy %s: %w", pName, err)
			}
		}
	}
	attached, err := p.iam.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName)})
	if err != nil && !isIAMNotFound(err) {
		return fmt.Errorf("list attached policies: %w", err)
	}
	if err == nil {
		for _, a := range attached.AttachedPolicies {
			if _, err := p.iam.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
				RoleName: aws.String(roleName), PolicyArn: a.PolicyArn,
			}); err != nil && !isIAMNotFound(err) {
				return fmt.Errorf("detach policy %s: %w", aws.ToString(a.PolicyArn), err)
			}
		}
	}

	_, err = p.iam.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	if err != nil && !isIAMNotFound(err) {
		return fmt.Errorf("delete role: %w", err)
	}
	return nil
}

func (p *Provider) destroySecurityGroups(ctx context.Context, name, env string) error {
	out, err := p.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + TagCluster), Values: []string{name}},
			{Name: aws.String("tag:" + TagEnv), Values: []string{env}},
			{Name: aws.String("tag:" + TagManaged), Values: []string{"true"}},
		},
	})
	if err != nil {
		return err
	}
	// First pass: strip rules so cross-SG references don't block deletion.
	for _, sg := range out.SecurityGroups {
		if len(sg.IpPermissions) > 0 {
			_, _ = p.ec2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
				GroupId: sg.GroupId, IpPermissions: sg.IpPermissions,
			})
		}
	}
	for _, sg := range out.SecurityGroups {
		if _, err := p.ec2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: sg.GroupId,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete sg %s: %w", aws.ToString(sg.GroupId), err)
		}
	}
	return nil
}

func (p *Provider) destroyRouteTable(ctx context.Context, name, env string) error {
	out, err := p.ec2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: roleFilters(name, env, "rtb")})
	if err != nil {
		return err
	}
	for _, rtb := range out.RouteTables {
		for _, assoc := range rtb.Associations {
			if assoc.Main != nil && *assoc.Main {
				continue
			}
			_, _ = p.ec2.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
				AssociationId: assoc.RouteTableAssociationId,
			})
		}
		if _, err := p.ec2.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
			RouteTableId: rtb.RouteTableId,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete rtb %s: %w", aws.ToString(rtb.RouteTableId), err)
		}
	}
	return nil
}

func (p *Provider) destroySubnet(ctx context.Context, name, env string) error {
	out, err := p.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: roleFilters(name, env, "subnet")})
	if err != nil {
		return err
	}
	for _, s := range out.Subnets {
		if _, err := p.ec2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: s.SubnetId}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete subnet %s: %w", aws.ToString(s.SubnetId), err)
		}
	}
	return nil
}

func (p *Provider) destroyIGW(ctx context.Context, name, env string) error {
	out, err := p.ec2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{Filters: roleFilters(name, env, "igw")})
	if err != nil {
		return err
	}
	for _, igw := range out.InternetGateways {
		for _, att := range igw.Attachments {
			_, _ = p.ec2.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
				InternetGatewayId: igw.InternetGatewayId, VpcId: att.VpcId,
			})
		}
		if _, err := p.ec2.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: igw.InternetGatewayId,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete igw %s: %w", aws.ToString(igw.InternetGatewayId), err)
		}
	}
	return nil
}

func (p *Provider) destroyVPC(ctx context.Context, name, env string) error {
	out, err := p.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: roleFilters(name, env, "vpc")})
	if err != nil {
		return err
	}
	for _, v := range out.Vpcs {
		if _, err := p.ec2.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: v.VpcId}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete vpc %s: %w", aws.ToString(v.VpcId), err)
		}
	}
	return nil
}

func (p *Provider) destroyParameters(ctx context.Context, name, env string) error {
	prefix := "/bonsai/" + name + "/" + env + "/"
	var names []string
	var token *string
	for {
		out, err := p.ssm.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path: aws.String(prefix), Recursive: aws.Bool(true), NextToken: token,
		})
		if err != nil {
			return err
		}
		for _, prm := range out.Parameters {
			names = append(names, aws.ToString(prm.Name))
		}
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	// DeleteParameters caps at 10 names per call.
	for i := 0; i < len(names); i += 10 {
		end := i + 10
		if end > len(names) {
			end = len(names)
		}
		if _, err := p.ssm.DeleteParameters(ctx, &ssm.DeleteParametersInput{Names: names[i:end]}); err != nil {
			return err
		}
	}
	return nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "InvalidVpcID.NotFound",
			"InvalidSubnetID.NotFound",
			"InvalidInternetGatewayID.NotFound",
			"InvalidRouteTableID.NotFound",
			"InvalidGroup.NotFound",
			"InvalidLaunchTemplateName.NotFoundException",
			"InvalidInstanceID.NotFound":
			return true
		}
	}
	return false
}

func isIAMNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nse *iamtypes.NoSuchEntityException
	return errors.As(err, &nse)
}
