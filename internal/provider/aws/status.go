package aws

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/badri/bonsai/internal/provider"
)

// Status is a read-only snapshot. It does not contact the k3s API — that
// would require pulling the kubeconfig and a round trip; not worth the
// latency for a first-look command. K3s version is read from a Parameter
// Store entry the server publishes (or "unknown" if absent).
func (p *Provider) Status(ctx context.Context, name, env string) (provider.PlatformStatus, error) {
	cp, err := p.findRunningInstance(ctx, name, env, "control-plane")
	if err != nil {
		return provider.PlatformStatus{}, err
	}

	workers, err := p.countRunningWorkers(ctx, name, env)
	if err != nil {
		return provider.PlatformStatus{}, err
	}

	ami := ""
	if cp != nil {
		ami = aws.ToString(cp.ImageId)
	}

	k3sVersion := p.readParam(ctx, "/bonsai/"+name+"/"+env+"/k3s_version")
	if k3sVersion == "" {
		k3sVersion = "unknown"
	}

	healthy := cp != nil &&
		cp.State != nil &&
		cp.State.Name == ec2types.InstanceStateNameRunning &&
		workers > 0

	return provider.PlatformStatus{
		Healthy:     healthy,
		WorkerCount: int(workers),
		K3sVersion:  k3sVersion,
		AMIID:       ami,
	}, nil
}

func (p *Provider) countRunningWorkers(ctx context.Context, name, env string) (int32, error) {
	asgName := "bonsai-" + name + "-" + env + "-workers"
	out, err := p.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return 0, err
	}
	if len(out.AutoScalingGroups) == 0 {
		return 0, nil
	}
	var inService int32
	for _, inst := range out.AutoScalingGroups[0].Instances {
		if inst.LifecycleState == asgtypes.LifecycleStateInService {
			inService++
		}
	}
	return inService, nil
}

func (p *Provider) readParam(ctx context.Context, key string) string {
	out, err := p.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(key), WithDecryption: aws.Bool(true),
	})
	if err != nil {
		var nf *ssmtypes.ParameterNotFound
		if errors.As(err, &nf) {
			return ""
		}
		return ""
	}
	return aws.ToString(out.Parameter.Value)
}

