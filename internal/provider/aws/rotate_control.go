package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// RotateControl replaces the control plane EC2 with a fresh one from the
// latest bonsai-node image, preserving state via an S3 snapshot.
//
//  1. SSM Run Command on the live control plane: stop k3s, tar
//     /var/lib/rancher/k3s, upload to s3://<bucket>/<name>/<env>/control-plane/latest.tar.gz
//  2. Terminate the old instance (wait until gone).
//  3. Launch a new instance from the latest AMI — server.sh.tmpl picks up
//     the snapshot from S3 on boot before starting k3s.
//  4. Re-associate the EIP so workers' CONTROL_PLANE_URL keeps resolving.
//  5. Wait for the new server to publish a fresh kubeconfig.
//
// Workers are not rotated — same IP, same token, same cluster identity.
// API downtime is the duration of steps 2–5 (typically 4–6 minutes).
func (p *Provider) RotateControl(ctx context.Context, name, env string) error {
	current, err := p.findRunningInstance(ctx, name, env, "control-plane")
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("no control plane found for %s/%s", name, env)
	}

	bucket := "bonsai-backups-" + p.accountID()
	if err := p.snapshotControlPlaneState(ctx, aws.ToString(current.InstanceId), name, env, bucket); err != nil {
		return fmt.Errorf("snapshot state: %w", err)
	}

	oldID := aws.ToString(current.InstanceId)
	if _, err := p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{oldID}}); err != nil {
		return fmt.Errorf("terminate old control plane: %w", err)
	}
	if err := p.waitForInstancesGone(ctx, []string{oldID}); err != nil {
		return err
	}

	eip, err := p.ensureControlEIP(ctx, name, env)
	if err != nil {
		return err
	}
	net, err := p.ensureVPC(ctx, name, env)
	if err != nil {
		return err
	}
	instanceProfile := iamName(name, env)

	newID, err := p.launchControlPlane(ctx, controlPlaneSpec{
		Name: name, Env: env,
		Net:             net,
		InstanceProfile: instanceProfile,
		ControlIP:       eip.PublicIP,
		BackupBucket:    bucket,
	})
	if err != nil {
		return fmt.Errorf("launch new control plane: %w", err)
	}
	if err := p.waitForInstanceState(ctx, newID, ec2types.InstanceStateNameRunning, 5*time.Minute); err != nil {
		return err
	}
	if err := p.associateControlEIP(ctx, eip, newID); err != nil {
		return fmt.Errorf("re-associate EIP: %w", err)
	}
	return p.waitForK3sReady(ctx, name, env)
}

// snapshotControlPlaneState issues an SSM Run Command that stops k3s, tars
// /var/lib/rancher/k3s, and uploads the tarball to S3 at the canonical path
// server.sh's restore-on-boot branch checks.
func (p *Provider) snapshotControlPlaneState(ctx context.Context, instanceID, name, env, bucket string) error {
	script := strings.Join([]string{
		"set -eu",
		"systemctl stop k3s || true",
		"tar -czf /tmp/state.tar.gz -C /var/lib/rancher k3s",
		fmt.Sprintf("aws s3 cp /tmp/state.tar.gz s3://%s/%s/%s/control-plane/snapshot-$(date -u +%%Y%%m%%dT%%H%%M%%SZ).tar.gz",
			bucket, name, env),
		fmt.Sprintf("aws s3 cp /tmp/state.tar.gz s3://%s/%s/%s/control-plane/latest.tar.gz",
			bucket, name, env),
		"rm /tmp/state.tar.gz",
	}, "\n")

	out, err := p.ssm.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters:   map[string][]string{"commands": {script}},
		Comment:      aws.String("bonsai: control plane state snapshot"),
		TimeoutSeconds: aws.Int32(600),
	})
	if err != nil {
		return fmt.Errorf("send snapshot command: %w", err)
	}
	commandID := aws.ToString(out.Command.CommandId)
	return p.waitForCommand(ctx, commandID, instanceID, 15*time.Minute)
}

func (p *Provider) waitForCommand(ctx context.Context, commandID, instanceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := p.ssm.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId: aws.String(commandID), InstanceId: aws.String(instanceID),
		})
		if err != nil {
			// SSM takes a beat to register the invocation; tolerate transient
			// "not found" before declaring failure.
			var nf *ssmtypes.InvocationDoesNotExist
			if errors.As(err, &nf) {
				time.Sleep(5 * time.Second)
				continue
			}
			return err
		}
		switch out.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			return nil
		case ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusCancelled,
			ssmtypes.CommandInvocationStatusTimedOut:
			return fmt.Errorf("ssm command %s on %s: %s\nstdout:\n%s\nstderr:\n%s",
				commandID, instanceID, out.Status,
				aws.ToString(out.StandardOutputContent),
				aws.ToString(out.StandardErrorContent))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("ssm command %s did not finish within %s", commandID, timeout)
}
