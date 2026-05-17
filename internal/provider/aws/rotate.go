package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// RotateWorkers replaces every worker node in-place via ASG instance refresh.
//
//   1. Resolve the target AMI ("latest" → discover, else use as-is).
//   2. Read the existing launch template, bump a new version with the new AMI.
//   3. Set it as the default version so the ASG picks it up.
//   4. StartInstanceRefresh with rolling strategy. Min healthy 50% so we
//      always keep at least half the workers running.
//
// Drain-before-terminate is handled by k3s' default node-shutdown grace
// period; kured covers the in-between case of OS-package reboots between
// rotations.
func (p *Provider) RotateWorkers(ctx context.Context, name, env, amiRef string) error {
	amiID, err := p.resolveAMIRef(ctx, amiRef)
	if err != nil {
		return fmt.Errorf("resolve AMI: %w", err)
	}

	ltName := "bonsai-" + name + "-" + env + "-worker"
	if err := p.bumpLaunchTemplateAMI(ctx, ltName, amiID); err != nil {
		return fmt.Errorf("bump launch template: %w", err)
	}

	asgName := "bonsai-" + name + "-" + env + "-workers"
	minHealthy := int32(50)
	_, err = p.asg.StartInstanceRefresh(ctx, &autoscaling.StartInstanceRefreshInput{
		AutoScalingGroupName: aws.String(asgName),
		Strategy:             asgtypes.RefreshStrategyRolling,
		Preferences: &asgtypes.RefreshPreferences{
			MinHealthyPercentage: aws.Int32(minHealthy),
			InstanceWarmup:       aws.Int32(180),
		},
	})
	if err != nil {
		return fmt.Errorf("start instance refresh: %w", err)
	}
	return nil
}

func (p *Provider) resolveAMIRef(ctx context.Context, ref string) (string, error) {
	if ref == "" || ref == "latest" {
		return p.resolveNodeAMI(ctx)
	}
	return ref, nil
}

func (p *Provider) bumpLaunchTemplateAMI(ctx context.Context, ltName, amiID string) error {
	// Pull the current default version, copy its data, swap the AMI, save as a
	// new version, mark default. Preserves user-data, SG, instance profile, etc.
	versions, err := p.ec2.DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateName: aws.String(ltName),
		Versions:           []string{"$Default"},
	})
	if err != nil {
		return err
	}
	if len(versions.LaunchTemplateVersions) == 0 {
		return fmt.Errorf("launch template %s has no default version", ltName)
	}
	cur := versions.LaunchTemplateVersions[0].LaunchTemplateData

	nv, err := p.ec2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateName: aws.String(ltName),
		LaunchTemplateData: copyLaunchTemplateData(cur, amiID),
	})
	if err != nil {
		return err
	}
	_, err = p.ec2.ModifyLaunchTemplate(ctx, &ec2.ModifyLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
		DefaultVersion:     aws.String(fmt.Sprint(*nv.LaunchTemplateVersion.VersionNumber)),
	})
	return err
}

// copyLaunchTemplateData maps the response shape to the request shape and
// swaps the AMI. Other fields carry over unchanged.
func copyLaunchTemplateData(src *ec2types.ResponseLaunchTemplateData, newAMI string) *ec2types.RequestLaunchTemplateData {
	dst := &ec2types.RequestLaunchTemplateData{
		ImageId:          aws.String(newAMI),
		InstanceType:     src.InstanceType,
		SecurityGroupIds: src.SecurityGroupIds,
		UserData:         src.UserData,
		KeyName:          src.KeyName,
	}
	if src.IamInstanceProfile != nil {
		dst.IamInstanceProfile = &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{
			Name: src.IamInstanceProfile.Name,
			Arn:  src.IamInstanceProfile.Arn,
		}
	}
	for _, ts := range src.TagSpecifications {
		dst.TagSpecifications = append(dst.TagSpecifications,
			ec2types.LaunchTemplateTagSpecificationRequest{
				ResourceType: ts.ResourceType,
				Tags:         ts.Tags,
			})
	}
	return dst
}
