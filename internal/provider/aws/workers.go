package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Workers run as an ASG. Each new instance boots, pulls the cluster token from
// Parameter Store, and joins via the standard k3s agent install. Scaling is a
// single SetDesiredCapacity. Rotation for image/k3s updates is an instance
// refresh against a new launch template version (see rotate.go).

const (
	defaultWorkerInstanceType = "t3.small"
)

func (p *Provider) ensureWorkers(ctx context.Context, name, env string, desired int32, net vpcInfra, instanceProfile, controlPlaneURL string) error {
	ltID, ltVersion, err := p.ensureLaunchTemplate(ctx, name, env, net, instanceProfile, controlPlaneURL)
	if err != nil {
		return fmt.Errorf("launch template: %w", err)
	}
	// ASG VPCZoneIdentifier accepts a comma-separated list — when HA is enabled
	// the workers spread across all available subnets/AZs for free.
	return p.ensureASG(ctx, name, env, ltID, ltVersion, desired, strings.Join(net.SubnetIDs, ","))
}

func (p *Provider) ensureLaunchTemplate(ctx context.Context, name, env string, net vpcInfra, instanceProfile, controlPlaneURL string) (id string, version string, err error) {
	ltName := "bonsai-" + name + "-" + env + "-worker"
	amiID, err := p.resolveNodeAMI(ctx)
	if err != nil {
		return "", "", err
	}
	userData, err := renderWorkerUserData(workerVars{
		Name: name, Env: env, Region: p.awsCfg.Region, ControlPlaneURL: controlPlaneURL,
	})
	if err != nil {
		return "", "", err
	}

	ltData := &ec2types.RequestLaunchTemplateData{
		ImageId:          aws.String(amiID),
		InstanceType:     ec2types.InstanceType(defaultWorkerInstanceType),
		SecurityGroupIds: []string{net.SGWorker},
		IamInstanceProfile: &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{
			Name: aws.String(instanceProfile),
		},
		UserData: aws.String(userData),
		TagSpecifications: []ec2types.LaunchTemplateTagSpecificationRequest{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: clusterTags(name, env, "worker")},
		},
	}

	existing, err := p.ec2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []string{ltName},
	})
	if err == nil && len(existing.LaunchTemplates) > 0 {
		// New version on every Provision call — cheap, ensures user-data drift
		// (admin CIDR changes, new AMI, etc.) flows through on the next rotate.
		nv, err := p.ec2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
			LaunchTemplateName: aws.String(ltName),
			LaunchTemplateData: ltData,
		})
		if err != nil {
			return "", "", fmt.Errorf("new launch template version: %w", err)
		}
		ltID := aws.ToString(nv.LaunchTemplateVersion.LaunchTemplateId)
		// Mark this as the default so ASG picks it up without further wiring.
		_, _ = p.ec2.ModifyLaunchTemplate(ctx, &ec2.ModifyLaunchTemplateInput{
			LaunchTemplateId: aws.String(ltID),
			DefaultVersion:   aws.String(fmt.Sprint(*nv.LaunchTemplateVersion.VersionNumber)),
		})
		return ltID, "$Default", nil
	}

	created, err := p.ec2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
		LaunchTemplateData: ltData,
		TagSpecifications: []ec2types.TagSpecification{
			tagSpec(ec2types.ResourceTypeLaunchTemplate, name, env, "worker-lt"),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("create launch template: %w", err)
	}
	return aws.ToString(created.LaunchTemplate.LaunchTemplateId), "$Default", nil
}

func (p *Provider) ensureASG(ctx context.Context, name, env, ltID, ltVersion string, desired int32, subnetID string) error {
	asgName := "bonsai-" + name + "-" + env + "-workers"

	out, err := p.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return err
	}
	if len(out.AutoScalingGroups) > 0 {
		_, err := p.asg.UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
			AutoScalingGroupName: aws.String(asgName),
			DesiredCapacity:      aws.Int32(desired),
			MinSize:              aws.Int32(0),
			MaxSize:              aws.Int32(maxInt32(desired, 10)),
			LaunchTemplate: &asgtypes.LaunchTemplateSpecification{
				LaunchTemplateId: aws.String(ltID),
				Version:          aws.String(ltVersion),
			},
		})
		return err
	}

	_, err = p.asg.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(asgName),
		MinSize:              aws.Int32(0),
		MaxSize:              aws.Int32(maxInt32(desired, 10)),
		DesiredCapacity:      aws.Int32(desired),
		VPCZoneIdentifier:    aws.String(subnetID),
		LaunchTemplate: &asgtypes.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(ltID),
			Version:          aws.String(ltVersion),
		},
		Tags: []asgtypes.Tag{
			{Key: aws.String(TagCluster), Value: aws.String(name), PropagateAtLaunch: aws.Bool(true)},
			{Key: aws.String(TagEnv), Value: aws.String(env), PropagateAtLaunch: aws.Bool(true)},
			{Key: aws.String(TagManaged), Value: aws.String("true"), PropagateAtLaunch: aws.Bool(true)},
			{Key: aws.String(TagRole), Value: aws.String("worker"), PropagateAtLaunch: aws.Bool(true)},
		},
	})
	if err != nil && !isAlreadyExists(err) && !isASGAlreadyExists(err) {
		return fmt.Errorf("create ASG: %w", err)
	}
	return nil
}

func isASGAlreadyExists(err error) bool {
	var ae *asgtypes.AlreadyExistsFault
	return errors.As(err, &ae)
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
