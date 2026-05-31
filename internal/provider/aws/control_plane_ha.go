package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// HA control plane: 3 k3s servers in an ASG spread across SubnetIDs, all
// attached to the NLB target group. server-ha.sh.tmpl handles the leader
// election via an SSM-lock so the ASG itself stays uniform.

const haControlPlaneSize = int32(3)

type controlPlaneHASpec struct {
	Name, Env       string
	Net             vpcInfra
	InstanceProfile string
	NLBDNS          string // empty in tailnet mode
	TargetGroupArn  string // empty in tailnet mode
	BackupBucket    string

	// Tailnet mode: when both fields set, render server-tailnet-ha user-data
	// and skip NLB target group registration.
	TailnetURL        string
	TailnetKeySSMPath string
	TailnetTag        string
}

func (s controlPlaneHASpec) tailnetMode() bool {
	return s.TailnetURL != "" && s.TailnetKeySSMPath != ""
}

func haControlPlaneLTName(name, env string) string {
	return "bonsai-" + name + "-" + env + "-control"
}

func haControlPlaneASGName(name, env string) string {
	return "bonsai-" + name + "-" + env + "-control"
}

func (p *Provider) ensureControlPlaneASG(ctx context.Context, spec controlPlaneHASpec) error {
	ltID, err := p.ensureControlPlaneLT(ctx, spec)
	if err != nil {
		return fmt.Errorf("control launch template: %w", err)
	}
	return p.ensureControlPlaneASGResource(ctx, spec, ltID)
}

func (p *Provider) ensureControlPlaneLT(ctx context.Context, spec controlPlaneHASpec) (string, error) {
	amiID, err := p.resolveNodeAMI(ctx)
	if err != nil {
		return "", err
	}
	var userData string
	if spec.tailnetMode() {
		userData, err = renderServerTailnetHAUserData(serverTailnetHAVars{
			Name: spec.Name, Env: spec.Env, Region: p.awsCfg.Region,
			TailnetURL: spec.TailnetURL, TailnetKeySSMPath: spec.TailnetKeySSMPath,
			TailnetTag: spec.TailnetTag,
		})
	} else {
		userData, err = renderServerHAUserData(serverHAVars{
			Name: spec.Name, Env: spec.Env, Region: p.awsCfg.Region,
			NLBDNS: spec.NLBDNS, BackupBucket: spec.BackupBucket,
		})
	}
	if err != nil {
		return "", err
	}
	ltData := &ec2types.RequestLaunchTemplateData{
		ImageId:          aws.String(amiID),
		InstanceType:     ec2types.InstanceType(defaultControlPlaneInstanceType),
		SecurityGroupIds: []string{spec.Net.SGServer},
		IamInstanceProfile: &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{
			Name: aws.String(spec.InstanceProfile),
		},
		UserData: aws.String(userData),
		// AL2023 launch templates default to IMDSv2 token-required; the
		// user-data script uses IMDSv2 explicitly so this matches.
		MetadataOptions: &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{
			HttpTokens:              ec2types.LaunchTemplateHttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(2),
			HttpEndpoint:            ec2types.LaunchTemplateInstanceMetadataEndpointStateEnabled,
		},
		TagSpecifications: []ec2types.LaunchTemplateTagSpecificationRequest{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: clusterTags(spec.Name, spec.Env, "control-plane")},
		},
	}

	ltName := haControlPlaneLTName(spec.Name, spec.Env)
	existing, err := p.ec2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []string{ltName},
	})
	if err == nil && len(existing.LaunchTemplates) > 0 {
		nv, err := p.ec2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
			LaunchTemplateName: aws.String(ltName),
			LaunchTemplateData: ltData,
		})
		if err != nil {
			return "", fmt.Errorf("new control LT version: %w", err)
		}
		ltID := aws.ToString(nv.LaunchTemplateVersion.LaunchTemplateId)
		_, _ = p.ec2.ModifyLaunchTemplate(ctx, &ec2.ModifyLaunchTemplateInput{
			LaunchTemplateId: aws.String(ltID),
			DefaultVersion:   aws.String(fmt.Sprint(*nv.LaunchTemplateVersion.VersionNumber)),
		})
		return ltID, nil
	}
	created, err := p.ec2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
		LaunchTemplateData: ltData,
		TagSpecifications: []ec2types.TagSpecification{
			tagSpec(ec2types.ResourceTypeLaunchTemplate, spec.Name, spec.Env, "control-lt"),
		},
	})
	if err != nil {
		return "", fmt.Errorf("create control LT: %w", err)
	}
	return aws.ToString(created.LaunchTemplate.LaunchTemplateId), nil
}

func (p *Provider) ensureControlPlaneASGResource(ctx context.Context, spec controlPlaneHASpec, ltID string) error {
	asgName := haControlPlaneASGName(spec.Name, spec.Env)
	zones := strings.Join(spec.Net.SubnetIDs, ",")

	out, err := p.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return err
	}
	if len(out.AutoScalingGroups) > 0 {
		_, err := p.asg.UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
			AutoScalingGroupName: aws.String(asgName),
			MinSize:              aws.Int32(haControlPlaneSize),
			MaxSize:              aws.Int32(haControlPlaneSize),
			DesiredCapacity:      aws.Int32(haControlPlaneSize),
			VPCZoneIdentifier:    aws.String(zones),
			LaunchTemplate: &asgtypes.LaunchTemplateSpecification{
				LaunchTemplateId: aws.String(ltID),
				Version:          aws.String("$Default"),
			},
		})
		return err
	}

	var tgARNs []string
	if spec.TargetGroupArn != "" {
		tgARNs = []string{spec.TargetGroupArn}
	}
	_, err = p.asg.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(asgName),
		MinSize:              aws.Int32(haControlPlaneSize),
		MaxSize:              aws.Int32(haControlPlaneSize),
		DesiredCapacity:      aws.Int32(haControlPlaneSize),
		VPCZoneIdentifier:    aws.String(zones),
		TargetGroupARNs:      tgARNs,
		// Spread across AZs so an AZ loss leaves a quorum (2 of 3) running.
		AvailabilityZones: nil, // VPCZoneIdentifier already encodes AZs via subnets
		LaunchTemplate: &asgtypes.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(ltID),
			Version:          aws.String("$Default"),
		},
		Tags: []asgtypes.Tag{
			{Key: aws.String(TagCluster), Value: aws.String(spec.Name), PropagateAtLaunch: aws.Bool(true)},
			{Key: aws.String(TagEnv), Value: aws.String(spec.Env), PropagateAtLaunch: aws.Bool(true)},
			{Key: aws.String(TagManaged), Value: aws.String("true"), PropagateAtLaunch: aws.Bool(true)},
			{Key: aws.String(TagRole), Value: aws.String("control-plane"), PropagateAtLaunch: aws.Bool(true)},
		},
	})
	if err != nil && !isASGAlreadyExists(err) {
		return fmt.Errorf("create control ASG: %w", err)
	}
	return nil
}

// haControlPlaneASGExists is used by RotateControl and Destroy to detect
// whether this cluster was provisioned in HA mode without requiring the
// caller to thread the flag through.
func (p *Provider) haControlPlaneASGExists(ctx context.Context, name, env string) (bool, error) {
	out, err := p.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{haControlPlaneASGName(name, env)},
	})
	if err != nil {
		return false, err
	}
	return len(out.AutoScalingGroups) > 0, nil
}

