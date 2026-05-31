package hetzner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// RotateControl replaces the Hetzner control-plane server, preserving cluster
// state via an SSH-mediated tarball round-trip:
//
//   1. SSH current control plane: stop k3s, tar /var/lib/rancher/k3s
//   2. pullFile the tarball to the operator's local FileSecretStore
//   3. Delete the Hetzner server
//   4. Create a new server from the latest baked image (or base ubuntu) with
//      restore.sh.tmpl cloud-init that does NOT auto-install k3s
//   5. Wait for SSH + the bonsai-restore-ready marker
//   6. pushFile the tarball, extract it into /var/lib/rancher
//   7. Run the k3s install script — installer detects the restored state
//      under /var/lib/rancher/k3s and continues from it
//   8. Reassign the cluster's floating IP to the new server
//   9. Poll the kubeconfig URL until the API is reachable
//
// Workers never reconnect — floating IP is preserved, server cert covers it
// via --tls-san, and the cluster identity (CA + node-token) is fully inside
// the snapshot. 5–10 minute API downtime.
func (p *Provider) RotateControl(ctx context.Context, name, env string) error {
	servers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "control-plane")},
	})
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		return fmt.Errorf("no control plane found for %s/%s", name, env)
	}
	current := servers[0]

	fip, err := p.findControlFloatingIP(ctx, name, env)
	if err != nil {
		return err
	}
	if fip == nil {
		return fmt.Errorf("no control floating IP found for %s/%s — was this cluster provisioned with the EIP-enabled version?", name, env)
	}
	controlIP := fip.IP.String()

	snapshotPath, err := p.snapshotControlState(ctx, name, env, controlIP)
	if err != nil {
		return fmt.Errorf("snapshot state: %w", err)
	}
	defer os.Remove(snapshotPath)

	if _, _, err := p.client.Server.DeleteWithResult(ctx, current); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete old control plane: %w", err)
	}

	sshKey, err := p.ensureSSHKey(ctx, name, env)
	if err != nil {
		return err
	}
	image, err := p.resolveWorkerImage(ctx, "latest") // same resolution as workers
	if err != nil {
		return err
	}
	hostPriv, hostPub, err := p.hostKeyMaterial(ctx, name, env)
	if err != nil {
		return err
	}
	userData, err := renderRestoreUserData(restoreVars{
		HostKeyPublic:          strings.TrimSpace(hostPub),
		HostKeyPrivateIndented: indentForCloudConfig(hostPriv, 4),
	})
	if err != nil {
		return err
	}
	res, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "bonsai-" + name + "-" + env + "-control",
		ServerType: &hcloud.ServerType{Name: defaultServerType},
		Image:      image,
		Location:   current.Datacenter.Location,
		SSHKeys:    []*hcloud.SSHKey{sshKey},
		UserData:   userData,
		Labels:     clusterLabels(name, env, "control-plane"),
	})
	if err != nil {
		return fmt.Errorf("create new control plane: %w", err)
	}
	newServer := res.Server

	newIP := newServer.PublicNet.IPv4.IP.String()
	if err := waitForSSH(ctx, newIP, sshReadyTimeout); err != nil {
		return fmt.Errorf("new control plane SSH: %w", err)
	}
	if err := p.restoreControlState(ctx, name, env, newIP, controlIP, snapshotPath); err != nil {
		return fmt.Errorf("restore state: %w", err)
	}
	if err := p.assignFloatingIP(ctx, fip, newServer); err != nil {
		return fmt.Errorf("reassign floating IP: %w", err)
	}
	return p.waitForK3sUp(ctx, name, env, controlIP)
}

// snapshotControlState SSHes into the live control plane, stops k3s, tars
// /var/lib/rancher/k3s, and copies the tarball back to a temp file on the
// operator's machine. Returns the local path.
func (p *Provider) snapshotControlState(ctx context.Context, name, env, ip string) (string, error) {
	client, err := p.sshClient(ctx, name, env, ip)
	if err != nil {
		return "", err
	}
	defer client.Close()

	if out, err := runSSH(client, "systemctl stop k3s || true"); err != nil {
		return "", fmt.Errorf("stop k3s: %w (%s)", err, out)
	}
	remoteTar := "/tmp/bonsai-state.tar.gz"
	if out, err := runSSH(client, "tar -czf "+remoteTar+" -C /var/lib/rancher k3s"); err != nil {
		return "", fmt.Errorf("tar state: %w (%s)", err, out)
	}

	localPath := filepath.Join(os.TempDir(), fmt.Sprintf("bonsai-%s-%s-state-%d.tar.gz", name, env, time.Now().Unix()))
	if _, err := pullFile(client, remoteTar, localPath); err != nil {
		return "", fmt.Errorf("pull tarball: %w", err)
	}
	// Tidy the remote — server is about to be deleted, but cheap insurance
	// if the rotation aborts before delete.
	_, _ = runSSH(client, "rm -f "+remoteTar)
	return localPath, nil
}

// restoreControlState pushes the snapshot to the new server, extracts it,
// and runs the k3s installer — which finds the restored state and resumes.
func (p *Provider) restoreControlState(ctx context.Context, name, env, sshIP, controlIP, snapshotPath string) error {
	client, err := p.sshClient(ctx, name, env, sshIP)
	if err != nil {
		return err
	}
	defer client.Close()

	// Wait for the restore-ready marker so apt-get update has finished.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := runSSH(client, "test -f /var/lib/bonsai-restore-ready"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	remoteTar := "/tmp/bonsai-state.tar.gz"
	if _, err := pushFile(client, snapshotPath, remoteTar); err != nil {
		return fmt.Errorf("push tarball: %w", err)
	}
	if out, err := runSSH(client, "tar -xzf "+remoteTar+" -C /var/lib/rancher && rm "+remoteTar); err != nil {
		return fmt.Errorf("extract: %w (%s)", err, out)
	}

	installCmd := fmt.Sprintf(
		`curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%q sh -s - server `+
			`--write-kubeconfig-mode=0644 --tls-san=%q --disable=traefik`,
		defaultK3sVersion, controlIP,
	)
	if out, err := runSSH(client, installCmd); err != nil {
		return fmt.Errorf("k3s install: %w (%s)", err, out)
	}
	return nil
}

// waitForK3sUp opens an SSH session to the floating IP (now bound to the new
// server) and polls for /etc/rancher/k3s/k3s.yaml — same readiness signal as
// initial Provision.
func (p *Provider) waitForK3sUp(ctx context.Context, name, env, ip string) error {
	deadline := time.Now().Add(k3sReadyTimeout)
	for time.Now().Before(deadline) {
		client, err := p.sshClient(ctx, name, env, ip)
		if err == nil {
			_, err := runSSH(client, "test -f /etc/rancher/k3s/k3s.yaml")
			client.Close()
			if err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("k3s did not come up on new control plane within %s", k3sReadyTimeout)
}

func (p *Provider) findControlFloatingIP(ctx context.Context, name, env string) (*hcloud.FloatingIP, error) {
	fips, err := p.client.FloatingIP.AllWithOpts(ctx, hcloud.FloatingIPListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "control-fip")},
	})
	if err != nil {
		return nil, err
	}
	if len(fips) == 0 {
		return nil, nil
	}
	return fips[0], nil
}
