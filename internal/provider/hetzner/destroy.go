package hetzner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Destroy removes every Bonsai-managed Hetzner resource for the cluster and
// wipes the local FileSecretStore directory. Idempotent: missing resources
// are not an error.
//
// Order:
//   1. Servers (control plane + workers)
//   2. Floating IP (unassign + delete)
//   3. SSH key
//   4. Local secret directory
func (p *Provider) Destroy(ctx context.Context, name, env string) error {
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
