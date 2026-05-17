package cluster

import "context"

// Bootstrap installs the Tier 1 in-cluster components, in order:
//
//   1. cert-manager              — CNPG dependency
//   2. CloudNativePG operator    — Postgres HA, PITR, S3 backups
//   3. CNPG Cluster CR           — 2 replicas, the actual database
//   4. Valkey                    — Redis-compatible KV (Helm chart)
//   5. system-upgrade-controller — autonomous k3s version bumps
//   6. kured                     — safe rolling reboots after apk upgrade
//
// Everything below is Helm-installed via the Go helm SDK — no shelling out.
//
// Idempotency: every install is `upgrade --install`. Bootstrap is safe to
// re-run on every `bonsai grow`.
func Bootstrap(ctx context.Context, kubeconfig []byte) error {
	// TODO Phase 1
	_ = ctx
	_ = kubeconfig
	return nil
}
