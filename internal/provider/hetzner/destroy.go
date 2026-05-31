package hetzner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Destroy removes every Bonsai-managed Hetzner resource for the cluster and
// wipes the local FileSecretStore directory. Idempotent: missing resources
// are not an error.
//
// Order (HA-aware):
//   1. Load balancer (so its private-network attachment doesn't block Network delete)
//   2. Servers (control plane + workers — also detach from private network)
//   3. Floating IP (unassign + delete)
//   4. Firewall
//   5. Network (after all attachments gone)
//   6. SSH key
//   7. Local secret directory
func (p *Provider) Destroy(ctx context.Context, name, env string) error {
	if err := p.destroyLoadBalancer(ctx, name, env); err != nil {
		return fmt.Errorf("destroy LB: %w", err)
	}

	servers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: clusterSelector(name, env)},
	})
	if err != nil {
		return fmt.Errorf("list servers: %w", err)
	}
	for _, s := range servers {
		if _, _, err := p.client.Server.DeleteWithResult(ctx, s); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete server %s: %w", s.Name, err)
		}
	}
	// Server.Delete is async; the firewall stays "in use" until each server
	// actually leaves. Poll until no Bonsai-labeled servers remain so the
	// firewall + network deletes that follow don't race.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		remaining, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
			ListOpts: hcloud.ListOpts{LabelSelector: clusterSelector(name, env)},
		})
		if err != nil {
			return fmt.Errorf("poll server deletion: %w", err)
		}
		if len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("servers still present after 3m: %d remaining", len(remaining))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	fips, err := p.client.FloatingIP.AllWithOpts(ctx, hcloud.FloatingIPListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: clusterSelector(name, env)},
	})
	if err != nil {
		return fmt.Errorf("list floating IPs: %w", err)
	}
	for _, fip := range fips {
		if fip.Server != nil {
			_, _, _ = p.client.FloatingIP.Unassign(ctx, fip)
		}
		if _, err := p.client.FloatingIP.Delete(ctx, fip); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete floating IP %d: %w", fip.ID, err)
		}
	}

	if err := p.destroyFirewall(ctx, name, env); err != nil {
		return fmt.Errorf("destroy firewall: %w", err)
	}
	if err := p.destroyNetwork(ctx, name, env); err != nil {
		return fmt.Errorf("destroy network: %w", err)
	}

	keys, err := p.client.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: clusterSelector(name, env)},
	})
	if err != nil {
		return fmt.Errorf("list ssh keys: %w", err)
	}
	for _, k := range keys {
		if _, err := p.client.SSHKey.Delete(ctx, k); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete ssh key %s: %w", k.Name, err)
		}
	}

	// Local secret directory. File store paths are <root>/<name>-<env>/<key>.
	dataDir := os.Getenv("BONSAI_DATA_DIR")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".bonsai")
	}
	_ = os.RemoveAll(filepath.Join(dataDir, name+"-"+env))

	return nil
}

func isNotFound(err error) bool {
	return err != nil && hcloud.IsError(err, hcloud.ErrorCodeNotFound)
}
