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
