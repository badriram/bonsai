package hetzner

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// HA clusters get a Hetzner Cloud Firewall applied to all server roles. Rules
// mirror the AWS SG posture (PR #21 + #22) so we don't re-learn the same bugs:
//
//   - 6443 TCP   from admin CIDR + LB private IPs (k8s API)
//   - 22   TCP   from admin CIDR (SSH for operator bootstrap)
//   - 2379-2380 TCP intra-network — etcd peer/client (HA REQUIRED; missing
//     this is what kept joiners out of quorum in early AWS smoke tests)
//   - 10250 TCP intra-network — kubelet
//   - 8472 UDP intra-network — flannel VXLAN (UDP not TCP — the other AWS
//     latent bug that masqueraded as "single-node works, multi-node doesn't")
//
// In tailnet mode all cluster mesh traffic flows over tailscale0 instead of
// the public interface; the intra-network allows are still safe to keep on
// in case k3s falls back to eth0.
const (
	bonsaiAdminCIDREnv = "BONSAI_ADMIN_CIDR"
)

// adminCIDR mirrors the AWS provider's helper. Empty when the operator hasn't
// set the env var.
func (p *Provider) adminCIDR() string {
	return getenv(bonsaiAdminCIDREnv)
}

func (p *Provider) ensureFirewall(ctx context.Context, name, env string, lbPrivateIP string) (*hcloud.Firewall, error) {
	fws, err := p.client.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "firewall")},
	})
	if err != nil {
		return nil, err
	}

	rules, err := p.haFirewallRules(lbPrivateIP)
	if err != nil {
		return nil, err
	}

	if len(fws) > 0 {
		fw := fws[0]
		_, _, err := p.client.Firewall.SetRules(ctx, fw, hcloud.FirewallSetRulesOpts{Rules: rules})
		if err != nil {
			return nil, fmt.Errorf("update firewall rules: %w", err)
		}
		return fw, nil
	}

	res, _, err := p.client.Firewall.Create(ctx, hcloud.FirewallCreateOpts{
		Name:   "bonsai-" + name + "-" + env,
		Labels: clusterLabels(name, env, "firewall"),
		Rules:  rules,
	})
	if err != nil {
		return nil, fmt.Errorf("create firewall: %w", err)
	}
	return res.Firewall, nil
}

// haFirewallRules builds the rule list. Pass an empty lbPrivateIP for tailnet
// mode (no LB → no LB→target rule).
func (p *Provider) haFirewallRules(lbPrivateIP string) ([]hcloud.FirewallRule, error) {
	_, privateNet, err := net.ParseCIDR(haNetworkRange)
	if err != nil {
		return nil, err
	}
	rules := []hcloud.FirewallRule{
		// kubelet — intra-cluster only
		mkRuleIn("tcp", "10250", []net.IPNet{*privateNet}, "kubelet intra-network"),
		// etcd peer + client — intra-cluster only
		mkRuleIn("tcp", "2379-2380", []net.IPNet{*privateNet}, "etcd intra-network"),
		// flannel VXLAN — UDP, intra-cluster only
		mkRuleIn("udp", "8472", []net.IPNet{*privateNet}, "flannel VXLAN intra-network"),
	}

	if adm := p.adminCIDR(); adm != "" {
		admCIDR, err := parseCIDR(adm)
		if err != nil {
			return nil, fmt.Errorf("invalid BONSAI_ADMIN_CIDR %q: %w", adm, err)
		}
		rules = append(rules,
			mkRuleIn("tcp", "6443", []net.IPNet{admCIDR}, "k8s API from admin"),
			mkRuleIn("tcp", "22", []net.IPNet{admCIDR}, "SSH from admin"),
		)
	}

	if lbPrivateIP != "" {
		lbCIDR, err := parseCIDR(lbPrivateIP + "/32")
		if err != nil {
			return nil, err
		}
		rules = append(rules,
			mkRuleIn("tcp", "6443", []net.IPNet{lbCIDR}, "k8s API from LB"),
		)
	}
	return rules, nil
}

func mkRuleIn(proto, port string, sources []net.IPNet, desc string) hcloud.FirewallRule {
	return hcloud.FirewallRule{
		Direction:   hcloud.FirewallRuleDirectionIn,
		Protocol:    hcloud.FirewallRuleProtocol(proto),
		Port:        hcloud.Ptr(port),
		SourceIPs:   sources,
		Description: hcloud.Ptr(desc),
	}
}

func parseCIDR(s string) (net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		return net.IPNet{}, err
	}
	return *ipNet, nil
}

// applyFirewall attaches the firewall to a server. Idempotent.
// Hetzner Cloud Firewalls only attach to a server's public NIC, and the
// public IPv4 is attached asynchronously after Server.Create returns —
// retry on private_net_only_server while the public IP is being wired up.
func (p *Provider) applyFirewall(ctx context.Context, fw *hcloud.Firewall, srv *hcloud.Server) error {
	res := hcloud.FirewallResource{Type: hcloud.FirewallResourceTypeServer, Server: &hcloud.FirewallResourceServer{ID: srv.ID}}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, _, err := p.client.Firewall.ApplyResources(ctx, fw, []hcloud.FirewallResource{res})
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "private_net_only_server") || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *Provider) destroyFirewall(ctx context.Context, name, env string) error {
	fws, err := p.client.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "firewall")},
	})
	if err != nil {
		return err
	}
	for _, fw := range fws {
		// Hetzner reports the firewall as "in use" until its applied_to list
		// fully clears after the linked servers are deleted. Explicitly remove
		// the resources first so Delete doesn't race on the consistency lag.
		cur, _, err := p.client.Firewall.GetByID(ctx, fw.ID)
		if err == nil && cur != nil && len(cur.AppliedTo) > 0 {
			resources := make([]hcloud.FirewallResource, 0, len(cur.AppliedTo))
			for _, r := range cur.AppliedTo {
				if r.Type == hcloud.FirewallResourceTypeServer && r.Server != nil {
					resources = append(resources, hcloud.FirewallResource{
						Type:   hcloud.FirewallResourceTypeServer,
						Server: &hcloud.FirewallResourceServer{ID: r.Server.ID},
					})
				}
			}
			if len(resources) > 0 {
				_, _, _ = p.client.Firewall.RemoveResources(ctx, fw, resources)
			}
		}
		// Retry the delete a few times — applied_to clears asynchronously.
		var lastErr error
		for i := 0; i < 12; i++ {
			if _, err := p.client.Firewall.Delete(ctx, fw); err == nil {
				lastErr = nil
				break
			} else {
				lastErr = err
				if !strings.Contains(err.Error(), "resource_in_use") {
					break
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		if lastErr != nil {
			return fmt.Errorf("delete firewall %d: %w", fw.ID, lastErr)
		}
	}
	return nil
}

func getenv(k string) string { return os.Getenv(k) }
