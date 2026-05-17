package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// controlEIP holds the Elastic IP that follows the control plane across
// rotations. Workers and operators connect to the EIP, so a control-plane
// rotation is invisible at the connection layer — just a few minutes of API
// downtime while the new instance restores state and starts k3s.
type controlEIP struct {
	AllocationID string
	PublicIP     string
}

// ensureControlEIP allocates an EIP for the cluster's control plane if one
// doesn't already exist. Idempotent via tag lookup.
func (p *Provider) ensureControlEIP(ctx context.Context, name, env string) (controlEIP, error) {
	out, err := p.ec2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: roleFilters(name, env, "control-eip"),
	})
	if err != nil {
		return controlEIP{}, err
	}
	if len(out.Addresses) > 0 {
		a := out.Addresses[0]
		return controlEIP{AllocationID: aws.ToString(a.AllocationId), PublicIP: aws.ToString(a.PublicIp)}, nil
	}
	created, err := p.ec2.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain: ec2types.DomainTypeVpc,
		TagSpecifications: []ec2types.TagSpecification{
			tagSpec(ec2types.ResourceTypeElasticIp, name, env, "control-eip"),
		},
	})
	if err != nil {
		return controlEIP{}, fmt.Errorf("allocate EIP: %w", err)
	}
	return controlEIP{
		AllocationID: aws.ToString(created.AllocationId),
		PublicIP:     aws.ToString(created.PublicIp),
	}, nil
}

// associateControlEIP attaches the EIP to the given control plane instance.
// AWS makes this idempotent: re-associating to the same instance is a no-op,
// and associating to a different instance disassociates the old one first.
func (p *Provider) associateControlEIP(ctx context.Context, eip controlEIP, instanceID string) error {
	_, err := p.ec2.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId:       aws.String(eip.AllocationID),
		InstanceId:         aws.String(instanceID),
		AllowReassociation: aws.Bool(true),
	})
	return err
}

// releaseControlEIP is called during Destroy.
func (p *Provider) releaseControlEIP(ctx context.Context, name, env string) error {
	out, err := p.ec2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: roleFilters(name, env, "control-eip"),
	})
	if err != nil {
		return err
	}
	for _, a := range out.Addresses {
		if a.AssociationId != nil {
			_, _ = p.ec2.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{
				AssociationId: a.AssociationId,
			})
		}
		if _, err := p.ec2.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
			AllocationId: a.AllocationId,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("release EIP %s: %w", aws.ToString(a.AllocationId), err)
		}
	}
	return nil
}
