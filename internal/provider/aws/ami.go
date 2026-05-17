package aws

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// resolveNodeAMI returns the AMI to launch nodes from.
//
// Resolution order:
//   1. An AMI tagged bonsai-node:latest=true that this account owns
//      (produced by `bonsai bake-ami`, once that lands).
//   2. Latest Amazon Linux 2023 x86_64 from Amazon's public AMIs.
//
// AL2023 is the Phase 1 default — aws-cli v2 ships with it and the k3s install
// script works out of the box. The bake-ami pipeline will replace this with a
// hardened Alpine + k3s image.
func (p *Provider) resolveNodeAMI(ctx context.Context) (string, error) {
	if id, ok, err := p.findBakedAMI(ctx); err != nil {
		return "", err
	} else if ok {
		return id, nil
	}
	return p.latestAmazonLinux(ctx)
}

func (p *Provider) findBakedAMI(ctx context.Context) (string, bool, error) {
	out, err := p.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:bonsai-node:latest"), Values: []string{"true"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("describe baked AMI: %w", err)
	}
	if len(out.Images) == 0 {
		return "", false, nil
	}
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.ToString(out.Images[i].CreationDate) > aws.ToString(out.Images[j].CreationDate)
	})
	return aws.ToString(out.Images[0].ImageId), true, nil
}

func (p *Provider) latestAmazonLinux(ctx context.Context) (string, error) {
	out, err := p.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"amazon"},
		Filters: []ec2types.Filter{
			{Name: aws.String("name"), Values: []string{"al2023-ami-2023.*-kernel-6.*-x86_64"}},
			{Name: aws.String("architecture"), Values: []string{"x86_64"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe AL2023: %w", err)
	}
	if len(out.Images) == 0 {
		return "", fmt.Errorf("no Amazon Linux 2023 AMI found in region")
	}
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.ToString(out.Images[i].CreationDate) > aws.ToString(out.Images[j].CreationDate)
	})
	return aws.ToString(out.Images[0].ImageId), nil
}
