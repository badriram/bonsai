package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Network layout (intentional): single public subnet, Internet Gateway, no NAT.
// See docs/architecture.md for rationale (cost + dependency topology).
//
// Security comes from security groups, not subnet privacy:
//   6443  (k3s API):  admin CIDR + worker SG
//   10250 (kubelet):  intra-cluster SG only
//   22    (SSH):      admin CIDR only (off by default — CIDR == "" means rule skipped)
//   egress: open

const (
	vpcCIDR      = "10.0.0.0/16"
	subnetCIDR   = "10.0.1.0/24"
	adminCIDREnv = "BONSAI_ADMIN_CIDR" // optional; gates 6443 + 22
)

type vpcInfra struct {
	VPCID    string
	SubnetID string
	IGWID    string
	SGServer string
	SGWorker string
}

// ensureVPC is idempotent: every step looks up by tag, creates if missing.
func (p *Provider) ensureVPC(ctx context.Context, name, env string) (vpcInfra, error) {
	vpcID, err := p.ensureVPCResource(ctx, name, env)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure vpc: %w", err)
	}
	igwID, err := p.ensureInternetGateway(ctx, name, env, vpcID)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure igw: %w", err)
	}
	subnetID, err := p.ensureSubnet(ctx, name, env, vpcID)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure subnet: %w", err)
	}
	if err := p.ensureRouteTable(ctx, name, env, vpcID, subnetID, igwID); err != nil {
		return vpcInfra{}, fmt.Errorf("ensure route table: %w", err)
	}
	sgServer, sgWorker, err := p.ensureSecurityGroups(ctx, name, env, vpcID)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure security groups: %w", err)
	}
	return vpcInfra{VPCID: vpcID, SubnetID: subnetID, IGWID: igwID, SGServer: sgServer, SGWorker: sgWorker}, nil
}

func (p *Provider) ensureVPCResource(ctx context.Context, name, env string) (string, error) {
	out, err := p.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: roleFilters(name, env, "vpc")})
	if err != nil {
		return "", err
	}
	if len(out.Vpcs) > 0 {
		return aws.ToString(out.Vpcs[0].VpcId), nil
	}
	created, err := p.ec2.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String(vpcCIDR),
		TagSpecifications: []ec2types.TagSpecification{tagSpec(ec2types.ResourceTypeVpc, name, env, "vpc")},
	})
	if err != nil {
		return "", err
	}
	vpcID := aws.ToString(created.Vpc.VpcId)
	// DNS hostnames are required for `hostname -f` in user-data to resolve sanely.
	if _, err := p.ec2.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return "", fmt.Errorf("enable dns hostnames: %w", err)
	}
	return vpcID, nil
}

func (p *Provider) ensureInternetGateway(ctx context.Context, name, env, vpcID string) (string, error) {
	out, err := p.ec2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{Filters: roleFilters(name, env, "igw")})
	if err != nil {
		return "", err
	}
	if len(out.InternetGateways) > 0 {
		igw := out.InternetGateways[0]
		igwID := aws.ToString(igw.InternetGatewayId)
		for _, att := range igw.Attachments {
			if aws.ToString(att.VpcId) == vpcID {
				return igwID, nil
			}
		}
		_, err := p.ec2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
		})
		return igwID, err
	}
	created, err := p.ec2.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []ec2types.TagSpecification{tagSpec(ec2types.ResourceTypeInternetGateway, name, env, "igw")},
	})
	if err != nil {
		return "", err
	}
	igwID := aws.ToString(created.InternetGateway.InternetGatewayId)
	if _, err := p.ec2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
	}); err != nil {
		return "", err
	}
	return igwID, nil
}

func (p *Provider) ensureSubnet(ctx context.Context, name, env, vpcID string) (string, error) {
	out, err := p.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: roleFilters(name, env, "subnet")})
	if err != nil {
		return "", err
	}
	if len(out.Subnets) > 0 {
		return aws.ToString(out.Subnets[0].SubnetId), nil
	}
	// Pick the first AZ in the region — Phase 1 is single-AZ; Phase 3 spreads
	// the HA control plane across three.
	azs, err := p.ec2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	if err != nil || len(azs.AvailabilityZones) == 0 {
		return "", fmt.Errorf("no availability zones: %w", err)
	}
	created, err := p.ec2.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(vpcID),
		CidrBlock:         aws.String(subnetCIDR),
		AvailabilityZone:  azs.AvailabilityZones[0].ZoneName,
		TagSpecifications: []ec2types.TagSpecification{tagSpec(ec2types.ResourceTypeSubnet, name, env, "subnet")},
	})
	if err != nil {
		return "", err
	}
	subnetID := aws.ToString(created.Subnet.SubnetId)
	// Auto-assign public IPs — workers and control plane both live in this subnet
	// and need direct internet egress without a NAT.
	if _, err := p.ec2.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(subnetID),
		MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return "", fmt.Errorf("enable public IP: %w", err)
	}
	return subnetID, nil
}

func (p *Provider) ensureRouteTable(ctx context.Context, name, env, vpcID, subnetID, igwID string) error {
	out, err := p.ec2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: roleFilters(name, env, "rtb")})
	if err != nil {
		return err
	}
	var rtbID string
	if len(out.RouteTables) > 0 {
		rtbID = aws.ToString(out.RouteTables[0].RouteTableId)
	} else {
		created, err := p.ec2.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
			VpcId:             aws.String(vpcID),
			TagSpecifications: []ec2types.TagSpecification{tagSpec(ec2types.ResourceTypeRouteTable, name, env, "rtb")},
		})
		if err != nil {
			return err
		}
		rtbID = aws.ToString(created.RouteTable.RouteTableId)
	}
	// Default route → IGW. CreateRoute is not idempotent so swallow the duplicate.
	if _, err := p.ec2.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtbID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(igwID),
	}); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("create default route: %w", err)
	}
	if _, err := p.ec2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtbID), SubnetId: aws.String(subnetID),
	}); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("associate route table: %w", err)
	}
	return nil
}

func (p *Provider) ensureSecurityGroups(ctx context.Context, name, env, vpcID string) (server, worker string, err error) {
	server, err = p.ensureSG(ctx, name, env, vpcID, "sg-server", "Bonsai k3s server SG")
	if err != nil {
		return "", "", err
	}
	worker, err = p.ensureSG(ctx, name, env, vpcID, "sg-worker", "Bonsai k3s worker SG")
	if err != nil {
		return "", "", err
	}

	adminCIDR := p.adminCIDR()
	// Server ingress: 6443 from workers + admin; 10250 intra-cluster.
	rules := []ec2types.IpPermission{
		intraSG(6443, 6443, server),
		intraSG(6443, 6443, worker),
		intraSG(10250, 10250, server),
		intraSG(10250, 10250, worker),
		// flannel VXLAN for inter-node pod traffic
		intraSG(8472, 8472, server),
		intraSG(8472, 8472, worker),
	}
	if adminCIDR != "" {
		rules = append(rules, cidrRule(6443, 6443, adminCIDR), cidrRule(22, 22, adminCIDR))
	}
	if err := p.authorizeIngress(ctx, server, rules); err != nil {
		return "", "", fmt.Errorf("server SG rules: %w", err)
	}

	// Worker ingress: 10250 + 8472 intra-cluster only.
	if err := p.authorizeIngress(ctx, worker, []ec2types.IpPermission{
		intraSG(10250, 10250, server),
		intraSG(10250, 10250, worker),
		intraSG(8472, 8472, server),
		intraSG(8472, 8472, worker),
	}); err != nil {
		return "", "", fmt.Errorf("worker SG rules: %w", err)
	}
	return server, worker, nil
}

func (p *Provider) ensureSG(ctx context.Context, name, env, vpcID, role, desc string) (string, error) {
	out, err := p.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: roleFilters(name, env, role)})
	if err != nil {
		return "", err
	}
	if len(out.SecurityGroups) > 0 {
		return aws.ToString(out.SecurityGroups[0].GroupId), nil
	}
	created, err := p.ec2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         aws.String("bonsai-" + name + "-" + env + "-" + role),
		Description:       aws.String(desc),
		VpcId:             aws.String(vpcID),
		TagSpecifications: []ec2types.TagSpecification{tagSpec(ec2types.ResourceTypeSecurityGroup, name, env, role)},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(created.GroupId), nil
}

func (p *Provider) authorizeIngress(ctx context.Context, sgID string, rules []ec2types.IpPermission) error {
	_, err := p.ec2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID), IpPermissions: rules,
	})
	if err != nil && !isAlreadyExists(err) {
		return err
	}
	return nil
}

func (p *Provider) adminCIDR() string {
	if p.envLookup != nil {
		return p.envLookup(adminCIDREnv)
	}
	return ""
}

func intraSG(from, to int32, sgID string) ec2types.IpPermission {
	return ec2types.IpPermission{
		IpProtocol:       aws.String("tcp"),
		FromPort:         aws.Int32(from),
		ToPort:           aws.Int32(to),
		UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(sgID)}},
	}
}

func cidrRule(from, to int32, cidr string) ec2types.IpPermission {
	return ec2types.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int32(from),
		ToPort:     aws.Int32(to),
		IpRanges:   []ec2types.IpRange{{CidrIp: aws.String(cidr)}},
	}
}
