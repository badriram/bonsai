package hetzner

import (
	"context"
	"fmt"
)

// Verbs not yet implemented for Hetzner. Tracked for Phase 2.1+.
//
// BakeImage: Hetzner produces an Image via the Snapshot API on a stopped
//            server. The flow mirrors AWS bake-image — launch a builder
//            server, run provisioning user-data, shutdown, hcloud Snapshot
//            API call, tag with bonsai-node:latest label, prune older
//            snapshots.
//
// RotateWorkers: replace worker servers one at a time. Drain the k8s node,
//                delete the Hetzner server, create a fresh one. No ASG
//                primitive to lean on — we do the iteration ourselves.
//
// RotateControl: SSH to current control plane, snapshot /var/lib/rancher/k3s
//                via tar, push to local store or external S3. Delete server.
//                Create new from latest snapshot image. Reassign floating IP.
//                Restore-on-boot logic in server.sh.tmpl will pick up state.

func (p *Provider) BakeImage(ctx context.Context, k3sVersion string) (string, error) {
	return "", fmt.Errorf("hetzner: BakeImage not yet implemented (Phase 2.1)")
}

func (p *Provider) RotateWorkers(ctx context.Context, name, env, imageRef string) error {
	return fmt.Errorf("hetzner: RotateWorkers not yet implemented (Phase 2.1)")
}

func (p *Provider) RotateControl(ctx context.Context, name, env string) error {
	return fmt.Errorf("hetzner: RotateControl not yet implemented (Phase 2.1)")
}
