package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Phase 1: single EC2 in the public subnet, k3s server with embedded SQLite.
// Phase 3 replaces this with a 3-node ASG behind an NLB, embedded etcd.

const (
	defaultControlPlaneInstanceType = "t3.small"
)

// ensureControlPlane returns the public DNS name of the running k3s server.
// Idempotent: looks up an instance tagged role=control-plane; launches one if
// missing; returns the existing one if already running.
func (p *Provider) ensureControlPlane(ctx context.Context, name, env string, net vpcInfra, instanceProfile string) (publicDNS string, err error) {
	existing, err := p.findRunningInstance(ctx, name, env, "control-plane")
	if err != nil {
		return "", err
	}
	if existing != nil {
		return aws.ToString(existing.PublicDnsName), nil
	}

	amiID, err := p.resolveNodeAMI(ctx)
	if err != nil {
		return "", err
	}
	userData, err := renderServerUserData(serverVars{Name: name, Env: env, Region: p.awsCfg.Region})
	if err != nil {
		return "", err
	}

	out, err := p.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:            aws.String(amiID),
		InstanceType:       ec2types.InstanceType(defaultControlPlaneInstanceType),
		MinCount:           aws.Int32(1),
		MaxCount:           aws.Int32(1),
		SubnetId:           aws.String(net.SubnetID),
		SecurityGroupIds:   []string{net.SGServer},
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: aws.String(instanceProfile)},
		UserData:           aws.String(userData),
		TagSpecifications: []ec2types.TagSpecification{
			tagSpec(ec2types.ResourceTypeInstance, name, env, "control-plane"),
		},
	})
	if err != nil {
		return "", fmt.Errorf("run control plane: %w", err)
	}
	instanceID := aws.ToString(out.Instances[0].InstanceId)

	return p.waitForPublicDNS(ctx, instanceID)
}

func (p *Provider) findRunningInstance(ctx context.Context, name, env, role string) (*ec2types.Instance, error) {
	filters := append(roleFilters(name, env, role),
		ec2types.Filter{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}})
	out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters})
	if err != nil {
		return nil, err
	}
	for _, r := range out.Reservations {
		for i := range r.Instances {
			return &r.Instances[i], nil
		}
	}
	return nil, nil
}

func (p *Provider) waitForPublicDNS(ctx context.Context, instanceID string) (string, error) {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
		if err != nil {
			return "", err
		}
		if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
			dns := aws.ToString(out.Reservations[0].Instances[0].PublicDnsName)
			if dns != "" {
				return dns, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return "", fmt.Errorf("timed out waiting for public DNS on %s", instanceID)
}

// waitForK3sReady polls Parameter Store for the kubeconfig that server.sh
// writes once k3s is up. Simpler and more reliable than poking 6443 ourselves.
func (p *Provider) waitForK3sReady(ctx context.Context, name, env string) error {
	key := "/bonsai/" + name + "/" + env + "/kubeconfig"
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		_, err := p.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(key)})
		if err == nil {
			return nil
		}
		var nf *ssmtypes.ParameterNotFound
		if !errors.As(err, &nf) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("k3s did not publish kubeconfig within 10m")
}
