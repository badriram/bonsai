package provider

import (
	"context"

	"github.com/badri/bonsai/internal/config"
)

// PlatformProvider is the only contract the CLI knows about. Each cloud (AWS,
// Hetzner, DigitalOcean) implements it. The CLI never branches on provider
// beyond constructing the right one.
type PlatformProvider interface {
	Provision(ctx context.Context, cfg config.ClusterConfig) (PlatformOutputs, error)
	Destroy(ctx context.Context, name, env string) error
	Status(ctx context.Context, name, env string) (PlatformStatus, error)

	// RotateWorkers replaces every worker node in-place. amiRef is "latest"
	// (use the latest bonsai-node-tagged image), an explicit AMI ID, or
	// provider-specific reference (e.g. Hetzner snapshot ID).
	RotateWorkers(ctx context.Context, name, env, amiRef string) error

	// UpgradeK3s applies system-upgrade-controller Plan CRDs to bump k3s on
	// every node. The controller does the draining, binary swap, and uncordon;
	// Bonsai just publishes the desired version. version is the k3s release
	// tag, e.g. "v1.31.0+k3s1".
	UpgradeK3s(ctx context.Context, name, env, version string) error

	// UpgradeComponent re-runs the install for a single in-cluster component
	// against its currently pinned version. Use after bumping a version in
	// internal/cluster/charts.go. Valid components:
	//   cert-manager | cnpg | valkey | kured | system-upgrade-controller
	UpgradeComponent(ctx context.Context, name, env, component string) error
}

// PlatformOutputs is the same shape on every provider — that's the point.
type PlatformOutputs struct {
	ClusterEndpoint    string
	KubeconfigLocation string // e.g. ssm:///bonsai/foo/prod/kubeconfig
	PostgresURL        string
	KVURL              string
}

type PlatformStatus struct {
	Healthy     bool
	WorkerCount int
	K3sVersion  string
	AMIID       string
}
