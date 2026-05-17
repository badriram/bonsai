package aws

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// BakeImage produces a fresh bonsai-node AMI: launches an AL2023 builder
// instance, runs builder.sh.tmpl which installs k3s and shuts down, then
// CreateImage'es the stopped instance, tags it as bonsai-node:latest, and
// removes the latest tag from the previous image. Old AMIs (beyond the
// most recent 5) are deregistered along with their snapshots.
//
// Idempotent semantics are weaker here than for Provision: a bake either
// produces a new image or returns an error. Concurrent bakes would race on
// the "latest" tag — we guard against this by failing if another builder
// instance is already running.
const (
	defaultK3sBakeVersion = "v1.31.0+k3s1"
	builderInstanceType   = "t3.small"
	bakeAMITagLatest      = "bonsai-node:latest"
	bakeAMITagK3sVersion  = "bonsai-node:k3s-version"
	bakeAMITagBuiltAt     = "bonsai-node:built-at"
	bakeKeepN             = 5
)

func (p *Provider) BakeImage(ctx context.Context, k3sVersion string) (string, error) {
	if k3sVersion == "" {
		k3sVersion = defaultK3sBakeVersion
	}

	if existing, err := p.findBuilderInstance(ctx); err != nil {
		return "", err
	} else if existing != "" {
		return "", fmt.Errorf("another builder instance is already running (%s); wait or terminate it first", existing)
	}

	baseAMI, err := p.latestAmazonLinux(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve base AMI: %w", err)
	}
	subnetID, err := p.defaultPublicSubnet(ctx)
	if err != nil {
		return "", fmt.Errorf("find subnet for builder: %w", err)
	}

	userData, err := renderBuilderUserData(builderVars{K3sVersion: k3sVersion})
	if err != nil {
		return "", err
	}

	builderID, err := p.launchBuilder(ctx, baseAMI, subnetID, userData)
	if err != nil {
		return "", fmt.Errorf("launch builder: %w", err)
	}
	defer func() {
		// Always clean up the builder, even on failure. Snapshot is already
		// owned by us at this point if CreateImage succeeded.
		_, _ = p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{builderID}})
	}()

	if err := p.waitForInstanceState(ctx, builderID, ec2types.InstanceStateNameStopped, 20*time.Minute); err != nil {
		return "", fmt.Errorf("builder never stopped: %w", err)
	}

	imageID, err := p.snapshotBuilder(ctx, builderID, k3sVersion)
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	if err := p.waitForAMIAvailable(ctx, imageID); err != nil {
		return "", fmt.Errorf("AMI never available: %w", err)
	}

	if err := p.promoteLatestAMI(ctx, imageID); err != nil {
		return "", fmt.Errorf("promote latest: %w", err)
	}
	if err := p.pruneOldAMIs(ctx); err != nil {
		// Pruning is best-effort — log via wrapped error but don't fail the bake.
		return imageID, fmt.Errorf("bake succeeded (%s) but prune failed: %w", imageID, err)
	}

	return imageID, nil
}

func (p *Provider) findBuilderInstance(ctx context.Context) (string, error) {
	out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + TagManaged), Values: []string{"true"}},
			{Name: aws.String("tag:" + TagRole), Values: []string{"image-builder"}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	if err != nil {
		return "", err
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			return aws.ToString(inst.InstanceId), nil
		}
	}
	return "", nil
}

// defaultPublicSubnet returns any public subnet in the default VPC. The
// builder runs there transiently — we don't want to spin up a dedicated VPC
// for a 10-minute bake.
func (p *Provider) defaultPublicSubnet(ctx context.Context) (string, error) {
	vpcs, err := p.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{{Name: aws.String("is-default"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", err
	}
	if len(vpcs.Vpcs) == 0 {
		return "", fmt.Errorf("no default VPC in region %s — create one or set BONSAI_BUILDER_SUBNET", p.awsCfg.Region)
	}
	subnets, err := p.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{aws.ToString(vpcs.Vpcs[0].VpcId)}},
			{Name: aws.String("default-for-az"), Values: []string{"true"}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(subnets.Subnets) == 0 {
		return "", fmt.Errorf("no default subnets in default VPC")
	}
	return aws.ToString(subnets.Subnets[0].SubnetId), nil
}

func (p *Provider) launchBuilder(ctx context.Context, baseAMI, subnetID, userData string) (string, error) {
	out, err := p.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:                           aws.String(baseAMI),
		InstanceType:                      ec2types.InstanceType(builderInstanceType),
		MinCount:                          aws.Int32(1),
		MaxCount:                          aws.Int32(1),
		SubnetId:                          aws.String(subnetID),
		UserData:                          aws.String(userData),
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorStop,
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: aws.String(TagManaged), Value: aws.String("true")},
					{Key: aws.String(TagRole), Value: aws.String("image-builder")},
					{Key: aws.String("Name"), Value: aws.String("bonsai-image-builder")},
				},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Instances[0].InstanceId), nil
}

func (p *Provider) waitForInstanceState(ctx context.Context, id string, want ec2types.InstanceStateName, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			return err
		}
		if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
			st := out.Reservations[0].Instances[0].State
			if st != nil && st.Name == want {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("instance %s did not reach %s within %s", id, want, timeout)
}

func (p *Provider) snapshotBuilder(ctx context.Context, builderID, k3sVersion string) (string, error) {
	name := fmt.Sprintf("bonsai-node-%s-%s", time.Now().UTC().Format("20060102-150405"), k3sVersion)
	out, err := p.ec2.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId:  aws.String(builderID),
		Name:        aws.String(name),
		Description: aws.String("Bonsai node image with k3s " + k3sVersion + " preinstalled"),
		NoReboot:    aws.Bool(true), // already stopped
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeImage,
				Tags: []ec2types.Tag{
					{Key: aws.String(TagManaged), Value: aws.String("true")},
					{Key: aws.String(bakeAMITagK3sVersion), Value: aws.String(k3sVersion)},
					{Key: aws.String(bakeAMITagBuiltAt), Value: aws.String(time.Now().UTC().Format(time.RFC3339))},
					{Key: aws.String("Name"), Value: aws.String(name)},
				},
			},
			{
				ResourceType: ec2types.ResourceTypeSnapshot,
				Tags: []ec2types.Tag{
					{Key: aws.String(TagManaged), Value: aws.String("true")},
					{Key: aws.String("Name"), Value: aws.String(name)},
				},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.ImageId), nil
}

func (p *Provider) waitForAMIAvailable(ctx context.Context, amiID string) error {
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := p.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{amiID}})
		if err != nil {
			return err
		}
		if len(out.Images) > 0 && out.Images[0].State == ec2types.ImageStateAvailable {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
		}
	}
	return fmt.Errorf("AMI %s not available within 20m", amiID)
}

// promoteLatestAMI puts the bonsai-node:latest=true tag on the new image and
// removes it from any previous holder, so resolveNodeAMI returns the new one.
func (p *Provider) promoteLatestAMI(ctx context.Context, newID string) error {
	prev, err := p.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + bakeAMITagLatest), Values: []string{"true"}},
		},
	})
	if err != nil {
		return err
	}
	for _, img := range prev.Images {
		if aws.ToString(img.ImageId) == newID {
			continue
		}
		if _, err := p.ec2.DeleteTags(ctx, &ec2.DeleteTagsInput{
			Resources: []string{aws.ToString(img.ImageId)},
			Tags:      []ec2types.Tag{{Key: aws.String(bakeAMITagLatest)}},
		}); err != nil {
			return err
		}
	}
	_, err = p.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{newID},
		Tags:      []ec2types.Tag{{Key: aws.String(bakeAMITagLatest), Value: aws.String("true")}},
	})
	return err
}

// pruneOldAMIs deregisters bonsai-node AMIs older than the most recent N and
// deletes their backing snapshots. Keeps a rollback window.
func (p *Provider) pruneOldAMIs(ctx context.Context) error {
	out, err := p.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + TagManaged), Values: []string{"true"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return err
	}
	imgs := out.Images
	sort.Slice(imgs, func(i, j int) bool {
		return aws.ToString(imgs[i].CreationDate) > aws.ToString(imgs[j].CreationDate)
	})
	if len(imgs) <= bakeKeepN {
		return nil
	}
	for _, img := range imgs[bakeKeepN:] {
		var snapIDs []string
		for _, bdm := range img.BlockDeviceMappings {
			if bdm.Ebs != nil && bdm.Ebs.SnapshotId != nil {
				snapIDs = append(snapIDs, aws.ToString(bdm.Ebs.SnapshotId))
			}
		}
		if _, err := p.ec2.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: img.ImageId}); err != nil {
			return err
		}
		for _, sid := range snapIDs {
			_, _ = p.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(sid)})
		}
	}
	return nil
}
