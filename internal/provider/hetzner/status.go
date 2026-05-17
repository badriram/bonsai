package hetzner

import (
	"context"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/badri/bonsai/internal/cluster"
	"github.com/badri/bonsai/internal/provider"
)

// Status: read-only snapshot from Hetzner API + the locally stored kubeconfig.
// Mirrors the AWS implementation's contract.
func (p *Provider) Status(ctx context.Context, name, env string) (provider.PlatformStatus, error) {
	control, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "control-plane")},
	})
	if err != nil {
		return provider.PlatformStatus{}, err
	}
	workers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "worker")},
	})
	if err != nil {
		return provider.PlatformStatus{}, err
	}

	var image string
	healthy := false
	if len(control) > 0 {
		c := control[0]
		healthy = c.Status == hcloud.ServerStatusRunning && len(workers) > 0
		if c.Image != nil {
			image = c.Image.Name
		}
	}
	return provider.PlatformStatus{
		Healthy:     healthy,
		WorkerCount: len(workers),
		K3sVersion:  defaultK3sVersion, // pinned in code; no per-cluster discovery on Hetzner yet
		AMIID:       image,
	}, nil
}

// UpgradeK3s + UpgradeComponent are trivial delegations — the cluster package
// is provider-agnostic and only needs the kubeconfig.

func (p *Provider) UpgradeK3s(ctx context.Context, name, env, version string) error {
	kc, err := p.store.Read(ctx, secretKey(name, env, kubeconfigSecretKey))
	if err != nil {
		return err
	}
	return cluster.UpgradeK3s(ctx, []byte(kc), version)
}

func (p *Provider) UpgradeComponent(ctx context.Context, name, env, component string) error {
	kc, err := p.store.Read(ctx, secretKey(name, env, kubeconfigSecretKey))
	if err != nil {
		return err
	}
	return cluster.UpgradeComponent(ctx, cluster.Config{
		Kubeconfig: []byte(kc), Name: name, Env: env,
	}, component)
}
