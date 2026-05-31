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
	adminCIDREnv = "BONSAI_ADMIN_CIDR" // optional; gates 6443 + 22
)

type vpcInfra struct {
	VPCID     string
	SubnetIDs []string // length 1 for single-AZ, 3 for HA. SubnetIDs[0] is the primary.
	IGWID     string
	SGServer  string
	SGWorker  string
}

// PrimarySubnet returns the subnet single-AZ code paths use.
func (v vpcInfra) PrimarySubnet() string { return v.SubnetIDs[0] }

// ensureVPC is idempotent: every step looks up by tag, creates if missing.
// When haControl is true, three subnets are created spread across distinct
// AZs so a 3-node etcd control plane can survive an AZ failure.
func (p *Provider) ensureVPC(ctx context.Context, name, env string, haControl bool) (vpcInfra, error) {
	vpcID, err := p.ensureVPCResource(ctx, name, env)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure vpc: %w", err)
	}
	igwID, err := p.ensureInternetGateway(ctx, name, env, vpcID)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure igw: %w", err)
	}
	azCount := 1
	if haControl {
		azCount = 3
	}
	subnetIDs, err := p.ensureSubnets(ctx, name, env, vpcID, azCount)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure subnets: %w", err)
	}
	if err := p.ensureRouteTable(ctx, name, env, vpcID, subnetIDs, igwID); err != nil {
		return vpcInfra{}, fmt.Errorf("ensure route table: %w", err)
	}
	sgServer, sgWorker, err := p.ensureSecurityGroups(ctx, name, env, vpcID)
	if err != nil {
		return vpcInfra{}, fmt.Errorf("ensure security groups: %w", err)
	}
	return vpcInfra{VPCID: vpcID, SubnetIDs: subnetIDs, IGWID: igwID, SGServer: sgServer, SGWorker: sgWorker}, nil
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

// ensureSubnets returns N subnet IDs, one per distinct AZ. Existing subnets
// are reused; missing AZs get a fresh /24 from the VPC's 10.0.x.0/24 space
// (deterministic by AZ index so the same AZ always gets the same CIDR).
func (p *Provider) ensureSubnets(ctx context.Context, name, env, vpcID string, n int) ([]string, error) {
	existing, err := p.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: roleFilters(name, env, "subnet")})
	if err != nil {
		return nil, err
	}
	have := map[string]string{} // az → subnet ID
	for _, s := range existing.Subnets {
		have[aws.ToString(s.AvailabilityZone)] = aws.ToString(s.SubnetId)
	}

	azs, err := p.ec2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	if err != nil || len(azs.AvailabilityZones) < n {
		return nil, fmt.Errorf("need %d AZs, region has %d: %w", n, len(azs.AvailabilityZones), err)
	}

	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		az := aws.ToString(azs.AvailabilityZones[i].ZoneName)
		if id, ok := have[az]; ok {
			ids = append(ids, id)
			continue
		}
		cidr := fmt.Sprintf("10.0.%d.0/24", i+1)
		created, err := p.ec2.CreateSubnet(ctx, &ec2.CreateSubnetInput{
			VpcId:             aws.String(vpcID),
			CidrBlock:         aws.String(cidr),
			AvailabilityZone:  aws.String(az),
			TagSpecifications: []ec2types.TagSpecification{tagSpec(ec2types.ResourceTypeSubnet, name, env, "subnet")},
		})
		if err != nil {
			return nil, fmt.Errorf("create subnet in %s: %w", az, err)
		}
		id := aws.ToString(created.Subnet.SubnetId)
		// Auto-assign public IPs — workers and control plane both live in these
		// subnets and need direct internet egress without a NAT.
		if _, err := p.ec2.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId:            aws.String(id),
			MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
		}); err != nil {
			return nil, fmt.Errorf("enable public IP on %s: %w", id, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (p *Provider) ensureRouteTable(ctx context.Context, name, env, vpcID string, subnetIDs []string, igwID string) error {
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
	for _, subnetID := range subnetIDs {
		if _, err := p.ec2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(rtbID), SubnetId: aws.String(subnetID),
		}); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("associate route table to %s: %w", subnetID, err)
		}
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
	// Reconcile previously-added admin-CIDR rules: AuthorizeSecurityGroupIngress
	// is additive only. Without this, every re-run from a new IP would leave
	// the old CIDR allowed forever. We tag rules we add with the bonsaiAdminDesc
	// description and revoke any tagged rules whose CIDR doesn't match the
	// current one (handles both IP drift and switching to tailnet mode where
	// adminCIDR is empty — all tagged rules get revoked).
	if err := p.reconcileAdminCIDRRules(ctx, server, adminCIDR); err != nil {
		return "", "", fmt.Errorf("reconcile admin CIDR rules: %w", err)
	}
	// Server ingress: 6443 from workers + admin; 10250 intra-cluster; etcd
	// peer/client ports intra-server only; flannel VXLAN for pod traffic.
	rules := []ec2types.IpPermission{
		intraSG(6443, 6443, server),
		intraSG(6443, 6443, worker),
		intraSG(10250, 10250, server),
		intraSG(10250, 10250, worker),
		// etcd client (2379) + peer (2380) — required for HA embedded etcd
		// quorum across the 3 control plane nodes.
		intraSG(2379, 2380, server),
		// flannel VXLAN for inter-node pod traffic — UDP, not TCP.
		intraSGUDP(8472, 8472, server),
		intraSGUDP(8472, 8472, worker),
		// NLB ENIs aren't in any SG; target health checks come from VPC IPs,
		// so allow 6443 from the VPC CIDR. Same-VPC clients are already
		// implicitly trusted; this just makes NLB health checks pass.
		cidrRule(6443, 6443, vpcCIDR, ""),
	}
	if adminCIDR != "" {
		rules = append(rules,
			cidrRule(6443, 6443, adminCIDR, bonsaiAdminDesc),
			cidrRule(22, 22, adminCIDR, bonsaiAdminDesc),
		)
	}
	if err := p.authorizeIngress(ctx, server, rules); err != nil {
		return "", "", fmt.Errorf("server SG rules: %w", err)
	}

	// Worker ingress: 10250 + 8472 intra-cluster only.
	if err := p.authorizeIngress(ctx, worker, []ec2types.IpPermission{
		intraSG(10250, 10250, server),
		intraSG(10250, 10250, worker),
		intraSGUDP(8472, 8472, server),
		intraSGUDP(8472, 8472, worker),
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

func intraSGUDP(from, to int32, sgID string) ec2types.IpPermission {
	return ec2types.IpPermission{
		IpProtocol:       aws.String("udp"),
		FromPort:         aws.Int32(from),
		ToPort:           aws.Int32(to),
		UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(sgID)}},
	}
}

func cidrRule(from, to int32, cidr, desc string) ec2types.IpPermission {
	r := ec2types.IpRange{CidrIp: aws.String(cidr)}
	if desc != "" {
		r.Description = aws.String(desc)
	}
	return ec2types.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int32(from),
		ToPort:     aws.Int32(to),
		IpRanges:   []ec2types.IpRange{r},
	}
}

// bonsaiAdminDesc marks SG rules Bonsai added to gate admin traffic
// (6443 + 22 from BONSAI_ADMIN_CIDR). reconcileAdminCIDRRules uses this to
// distinguish bonsai-managed rules from operator-added ones so re-runs from
// a new admin IP don't accumulate stale CIDRs.
const bonsaiAdminDesc = "bonsai-admin"

// reconcileAdminCIDRRules revokes any 6443/22 IpRange rules that carry the
// bonsaiAdminDesc description and whose CIDR no longer matches currentCIDR.
// Pass currentCIDR == "" to revoke all bonsai-admin rules (e.g. when the
// operator switches to tailnet mode and no longer wants any public CIDR).
// Rules without our description (e.g. operator-added manually) are left alone.
func (p *Provider) reconcileAdminCIDRRules(ctx context.Context, sgID, currentCIDR string) error {
	out, err := p.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{sgID},
	})
	if err != nil || len(out.SecurityGroups) == 0 {
		return err
	}
	var toRevoke []ec2types.IpPermission
	for _, perm := range out.SecurityGroups[0].IpPermissions {
		from := aws.ToInt32(perm.FromPort)
		if from != 6443 && from != 22 {
			continue
		}
		for _, r := range perm.IpRanges {
			if aws.ToString(r.Description) != bonsaiAdminDesc {
				continue
			}
			if aws.ToString(r.CidrIp) == currentCIDR {
				continue
			}
			toRevoke = append(toRevoke, ec2types.IpPermission{
				IpProtocol: perm.IpProtocol,
				FromPort:   perm.FromPort,
				ToPort:     perm.ToPort,
				IpRanges:   []ec2types.IpRange{{CidrIp: r.CidrIp, Description: r.Description}},
			})
		}
	}
	if len(toRevoke) == 0 {
		return nil
	}
	_, err = p.ec2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(sgID), IpPermissions: toRevoke,
	})
	return err
}
