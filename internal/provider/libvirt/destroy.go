package libvirt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/badriram/bonsai/internal/provider"
)

// Destroy tears down every libvirt resource Bonsai created for the cluster,
// then wipes the local FileSecretStore + VM pool. Idempotent: missing
// domains aren't errors.
func (p *Provider) Destroy(ctx context.Context, name, env string) error {
	prefix := fmt.Sprintf("bonsai-%s-%s-", name, env)

	listOut, err := virsh(ctx, p.uri, "list", "--all", "--name")
	if err != nil {
		return fmt.Errorf("virsh list: %w", err)
	}
	for _, dom := range strings.Split(strings.TrimSpace(string(listOut)), "\n") {
		dom = strings.TrimSpace(dom)
		if !strings.HasPrefix(dom, prefix) {
			continue
		}
		// destroy is forceful but idempotent if the domain is already off;
		// fold the "domain is not running" error into a no-op.
		if _, err := virsh(ctx, p.uri, "destroy", dom); err != nil && !strings.Contains(err.Error(), "not running") {
			return fmt.Errorf("virsh destroy %s: %w", dom, err)
		}
		if _, err := virsh(ctx, p.uri, "undefine", dom, "--nvram"); err != nil {
			return fmt.Errorf("virsh undefine %s: %w", dom, err)
		}
	}

	// Drop the per-cluster overlay pool. Base image stays in the shared
	// _images/ cache so the next grow doesn't re-download it.
	_ = os.RemoveAll(filepath.Join(p.dataDir, name+"-"+env))
	return nil
}

// Status returns a minimal snapshot: count of running VMs matching the
// cluster prefix, and whether the control plane is up. Richer fields land
// when libvirt state.json is fleshed out alongside #34.
func (p *Provider) Status(ctx context.Context, name, env string) (provider.PlatformStatus, error) {
	prefix := fmt.Sprintf("bonsai-%s-%s-", name, env)
	out, err := virsh(ctx, p.uri, "list", "--name")
	if err != nil {
		return provider.PlatformStatus{}, fmt.Errorf("virsh list: %w", err)
	}
	var control bool
	var workers int
	for _, dom := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dom = strings.TrimSpace(dom)
		if !strings.HasPrefix(dom, prefix) {
			continue
		}
		switch {
		case strings.HasSuffix(dom, "-control"):
			control = true
		case strings.Contains(dom, "-worker-"):
			workers++
		}
	}
	return provider.PlatformStatus{
		Healthy:     control,
		WorkerCount: workers,
	}, nil
}
