package aws

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// ensureIAM builds the instance profile that both control plane and workers
// assume. It grants exactly what the cluster needs at runtime — nothing more.
//
// - SSM: read the k3s join token, read/write cluster outputs under /bonsai/<name>/<env>/*
// - S3:  read/write CNPG WAL + base backups under the bonsai-backups bucket
//   (scoped to the cluster's prefix).
//
// IAM doesn't have AWS-tag-based lookup in the same way EC2 does, so we use a
// deterministic name (bonsai-<name>-<env>) and treat NoSuchEntity as "create".
func (p *Provider) ensureIAM(ctx context.Context, name, env, backupBucket string, extraSSMReadPaths ...string) (instanceProfileName string, err error) {
	roleName := iamName(name, env)
	if err := p.ensureRole(ctx, roleName, name, env); err != nil {
		return "", fmt.Errorf("ensure role: %w", err)
	}
	if err := p.putInlinePolicy(ctx, roleName, "ssm", ssmPolicy(name, env, extraSSMReadPaths...)); err != nil {
		return "", fmt.Errorf("ssm policy: %w", err)
	}
	if err := p.putInlinePolicy(ctx, roleName, "s3-backup", s3Policy(name, env, backupBucket)); err != nil {
		return "", fmt.Errorf("s3 policy: %w", err)
	}
	// AmazonSSMManagedInstanceCore enables SSM Run Command, which rotate-control
	// uses to take a state snapshot on the live control plane before terminating.
	if err := p.attachManagedPolicy(ctx, roleName, "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"); err != nil {
		return "", fmt.Errorf("attach SSM managed policy: %w", err)
	}
	if err := p.ensureInstanceProfile(ctx, roleName); err != nil {
		return "", fmt.Errorf("ensure instance profile: %w", err)
	}
	return roleName, nil
}

func (p *Provider) attachManagedPolicy(ctx context.Context, roleName, policyArn string) error {
	_, err := p.iam.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName: aws.String(roleName), PolicyArn: aws.String(policyArn),
	})
	return err
}

func iamName(name, env string) string { return "bonsai-" + name + "-" + env }

func (p *Provider) ensureRole(ctx context.Context, roleName, cluster, env string) error {
	_, err := p.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err == nil {
		return nil
	}
	var nse *iamtypes.NoSuchEntityException
	if !errors.As(err, &nse) {
		return err
	}
	_, err = p.iam.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(ec2AssumeRolePolicy),
		Description:              aws.String("Bonsai EC2 role for cluster " + cluster + "/" + env),
		Tags: []iamtypes.Tag{
			{Key: aws.String(TagCluster), Value: aws.String(cluster)},
			{Key: aws.String(TagEnv), Value: aws.String(env)},
			{Key: aws.String(TagManaged), Value: aws.String("true")},
		},
	})
	return err
}

func (p *Provider) putInlinePolicy(ctx context.Context, roleName, policyName, doc string) error {
	_, err := p.iam.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(doc),
	})
	return err
}

func (p *Provider) ensureInstanceProfile(ctx context.Context, roleName string) error {
	created := false
	_, err := p.iam.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(roleName)})
	if err != nil {
		var nse *iamtypes.NoSuchEntityException
		if !errors.As(err, &nse) {
			return err
		}
		if _, err := p.iam.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(roleName),
		}); err != nil {
			return err
		}
		created = true
	}
	_, err = p.iam.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
		RoleName:            aws.String(roleName),
	})
	var lee *iamtypes.LimitExceededException
	roleNewlyAttached := err == nil
	if err != nil && !errors.As(err, &lee) {
		// LimitExceeded is the error when the role is already attached
		// (an instance profile holds at most one role).
		return err
	}
	// IAM is eventually consistent across services. New (or freshly
	// role-attached) instance profiles are routinely rejected by EC2/ASG
	// with "Invalid IAM Instance Profile name" for the first 30-60s. Wait
	// for the profile to become visible to EC2 by polling DescribeInstances
	// with a dry-run validate, then sleeping a small buffer.
	if created || roleNewlyAttached {
		if err := p.waitForInstanceProfilePropagation(ctx, roleName); err != nil {
			return fmt.Errorf("wait for instance profile %s propagation: %w", roleName, err)
		}
	}
	return nil
}

// waitForInstanceProfilePropagation polls until the IAM service confirms the
// role is attached, then sleeps a buffer for the EC2/ASG side caches. AWS
// has no direct probe for "is this profile visible to EC2" so the sleep is
// empirical (~15s covers the steady-state case).
func (p *Provider) waitForInstanceProfilePropagation(ctx context.Context, roleName string) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := p.iam.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(roleName),
		})
		if err == nil && out.InstanceProfile != nil && len(out.InstanceProfile.Roles) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
	}
	return nil
}

const ec2AssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "ec2.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}`

func ssmPolicy(name, env string, extraReadPaths ...string) string {
	prefix := "/bonsai/" + name + "/" + env + "/*"
	statements := []any{
		map[string]any{
			"Effect":   "Allow",
			"Action":   []string{"ssm:GetParameter", "ssm:GetParameters", "ssm:PutParameter"},
			"Resource": "arn:aws:ssm:*:*:parameter" + prefix,
		},
	}
	for _, path := range extraReadPaths {
		statements = append(statements, map[string]any{
			"Effect":   "Allow",
			"Action":   []string{"ssm:GetParameter"},
			"Resource": "arn:aws:ssm:*:*:parameter" + path,
		})
	}
	doc := map[string]any{
		"Version":   "2012-10-17",
		"Statement": append(statements,
			map[string]any{
				// Decrypt SecureString values written under the cluster prefix.
				"Effect":   "Allow",
				"Action":   []string{"kms:Decrypt"},
				"Resource": "*",
				"Condition": map[string]any{
					"StringEquals": map[string]any{"kms:ViaService": "ssm.*.amazonaws.com"},
				},
			},
		),
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func s3Policy(name, env, bucket string) string {
	prefix := name + "/" + env + "/*"
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Effect":   "Allow",
				"Action":   []string{"s3:ListBucket", "s3:GetBucketLocation"},
				"Resource": "arn:aws:s3:::" + bucket,
				"Condition": map[string]any{
					"StringLike": map[string]any{"s3:prefix": []string{name + "/" + env + "/*"}},
				},
			},
			map[string]any{
				"Effect":   "Allow",
				"Action":   []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"},
				"Resource": "arn:aws:s3:::" + bucket + "/" + prefix,
			},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}
