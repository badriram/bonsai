package libvirt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/badriram/bonsai/internal/provider"
	"github.com/badriram/bonsai/internal/state"
	"github.com/badriram/bonsai/internal/tailnet"
)

// Destroy tears down every libvirt resource Bonsai created for the cluster,
// then wipes the local FileSecretStore + VM pool. Idempotent: missing
// domains aren't errors.
//
// When the cluster was provisioned in tailnet mode AND the operator
// supplied a tailnet.api_token_file in bonsai.yaml, also delete the
// tailnet device records via the Tailscale management API. We read the
// last-known config from state.json — destroy doesn't take a
// ClusterConfig today, and operators may not have the original
// bonsai.yaml at hand months after grow.
func (p *Provider) Destroy(ctx context.Context, name, env string) error {
	prefix := fmt.Sprintf("bonsai-%s-%s-", name, env)

	// Prune tailnet device registrations before destroying VMs — if we tore
	// the VMs down first the API call would still work, but doing it here
	// fails-fast on credential issues before any destructive action.
	p.pruneTailnetIfConfigured(ctx, name, env, prefix)

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

// pruneTailnetIfConfigured is best-effort: any error is printed but does
// not fail destroy. Operators who skipped api_token_file (or never used
// tailnet for this cluster) hit the no-op path silently.
func (p *Provider) pruneTailnetIfConfigured(ctx context.Context, name, env, hostnamePrefix string) {
	st, err := state.Read(state.Path(p.dataDir, name, env))
	if err != nil || st == nil {
		return
	}
	if !st.Declared.TailnetMode() {
		return
	}
	tokenFile := st.Declared.TailnetAPITokenFile
	if tokenFile == "" {
		fmt.Fprintf(os.Stderr, "warning: cluster was in tailnet mode but tailnet.api_token_file is unset — prune devices manually at https://login.tailscale.com/admin/machines\n")
		return
	}
	deleted, err := tailnet.PruneFromFile(ctx, tokenFile, hostnamePrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: tailnet device prune failed: %v\n", err)
		return
	}
	if deleted > 0 {
		fmt.Fprintf(os.Stderr, "pruned %d tailnet device(s) matching %s*\n", deleted, hostnamePrefix)
	}
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
