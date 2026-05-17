package hetzner

import (
	"context"
	"fmt"
)

// RotateControl: SSH-snapshot of /var/lib/rancher/k3s, push to local store
// (or external S3), delete current server, create new from latest snapshot
// image, SSH-restore state, install k3s with existing state, reassign
// floating IP. Substantial enough to deserve its own PR; landing next.
func (p *Provider) RotateControl(ctx context.Context, name, env string) error {
	return fmt.Errorf("hetzner: RotateControl not yet implemented (Phase 2.1 follow-up)")
}
