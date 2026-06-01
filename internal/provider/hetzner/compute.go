package hetzner

import (
	"context"
	"fmt"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	bcfg "github.com/badriram/bonsai/internal/config"
)

func (p *Provider) ensureControlFloatingIP(ctx context.Context, name, env, location string) (*hcloud.FloatingIP, error) {
	fips, err := p.client.FloatingIP.AllWithOpts(ctx, hcloud.FloatingIPListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "control-fip")},
	})
	if err != nil {
		return nil, err
	}
	if len(fips) > 0 {
		return fips[0], nil
	}
	loc := &hcloud.Location{Name: location}
	res, _, err := p.client.FloatingIP.Create(ctx, hcloud.FloatingIPCreateOpts{
		Type:         hcloud.FloatingIPTypeIPv4,
		HomeLocation: loc,
		Description:  hcloud.Ptr("bonsai " + name + "/" + env + " control plane"),
		Labels:       clusterLabels(name, env, "control-fip"),
	})
	if err != nil {
		return nil, err
	}
	return res.FloatingIP, nil
}

func (p *Provider) assignFloatingIP(ctx context.Context, fip *hcloud.FloatingIP, server *hcloud.Server) error {
	if fip.Server != nil && fip.Server.ID == server.ID {
		return nil
	}
	if fip.Server != nil {
		if _, _, err := p.client.FloatingIP.Unassign(ctx, fip); err != nil {
			return fmt.Errorf("unassign current: %w", err)
		}
	}
	_, _, err := p.client.FloatingIP.Assign(ctx, fip, server)
	return err
}

func (p *Provider) ensureControlPlane(ctx context.Context, cfg bcfg.ClusterConfig, location string, sshKey *hcloud.SSHKey, controlIP string) (*hcloud.Server, error) {
	servers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(cfg.Name, cfg.Env, "control-plane")},
	})
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.Status == hcloud.ServerStatusRunning || s.Status == hcloud.ServerStatusStarting || s.Status == hcloud.ServerStatusInitializing {
			return s, nil
		}
	}

	hostPriv, hostPub, err := p.hostKeyMaterial(ctx, cfg.Name, cfg.Env)
	if err != nil {
		return nil, err
	}
	userData, err := renderServerUserData(serverVars{
		ControlIP:              controlIP,
		K3sVersion:             k3sVersionOrDefault(cfg.K3sVersion),
		HostKeyPublic:          strings.TrimSpace(hostPub),
		HostKeyPrivateIndented: indentForCloudConfig(hostPriv, 4),
	})
	if err != nil {
		return nil, err
	}
	serverType := cfg.ControlServerType
	if serverType == "" {
		serverType = defaultServerType
	}
	res, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "bonsai-" + cfg.Name + "-" + cfg.Env + "-control",
		ServerType: &hcloud.ServerType{Name: serverType},
		Image:      &hcloud.Image{Name: defaultControlImage},
		Location:   &hcloud.Location{Name: location},
		SSHKeys:    []*hcloud.SSHKey{sshKey},
		UserData:   userData,
		Labels:     clusterLabels(cfg.Name, cfg.Env, "control-plane"),
	})
	if err != nil {
		return nil, err
	}
	return res.Server, nil
}

// ensureWorkers brings the worker count to `desired`. Adds servers if short,
// trims the youngest first if long. No ASG concept on Hetzner — we manage
// individual servers.
func (p *Provider) ensureWorkers(ctx context.Context, cfg bcfg.ClusterConfig, location string, sshKey *hcloud.SSHKey, controlIP, token string, desired int) error {
	current, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(cfg.Name, cfg.Env, "worker")},
	})
	if err != nil {
		return err
	}
	if len(current) >= desired {
		// Phase 2: don't auto-trim. Operator can scale down via rotate/destroy
		// individual servers if needed.
		return nil
	}
	userData, err := renderWorkerUserData(workerVars{
		ControlIP: controlIP, K3sVersion: k3sVersionOrDefault(cfg.K3sVersion), Token: token,
	})
	if err != nil {
		return err
	}
	serverType := cfg.WorkerServerType
	if serverType == "" {
		serverType = defaultServerType
	}
	for i := len(current); i < desired; i++ {
		_, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
			Name:       fmt.Sprintf("bonsai-%s-%s-worker-%d", cfg.Name, cfg.Env, i+1),
			ServerType: &hcloud.ServerType{Name: serverType},
			Image:      &hcloud.Image{Name: defaultControlImage},
			Location:   &hcloud.Location{Name: location},
			SSHKeys:    []*hcloud.SSHKey{sshKey},
			UserData:   userData,
			Labels:     clusterLabels(cfg.Name, cfg.Env, "worker"),
		})
		if err != nil {
			return fmt.Errorf("create worker %d: %w", i+1, err)
		}
	}
	return nil
}
