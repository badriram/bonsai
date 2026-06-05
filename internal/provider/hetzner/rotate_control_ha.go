package hetzner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/badriram/bonsai/internal/secrets"
)

// rotateControlHA replaces every HA control plane server, one at a time,
// preserving etcd quorum throughout (2 of 3 stay up while one is rolling).
//
// No snapshot/restore needed — etcd quorum on the survivors carries cluster
// state. v1 trade-off: we don't drain pods before deletion (would require
// client-go eviction-API plumbing), so workloads on the doomed control
// plane get hard-evicted; k8s reschedules them on the other control planes
// or workers. For Bonsai's "small team, few stateful workloads on control
// plane" target, this is acceptable. Future PR can add drain.
//
// Etcd member cleanup: when the Hetzner server is deleted, k3s on the
// survivors notices the missing member after the etcd liveness timeout (~30s)
// and removes it from quorum. We sleep before launching the replacement so
// the new server joins a clean 2-member quorum, not one with a phantom dead
// member.
func (p *Provider) rotateControlHA(ctx context.Context, name, env string, servers []*hcloud.Server) error {
	if len(servers) < haControlSize {
		return fmt.Errorf("rotateControlHA called with %d servers, expected at least %d", len(servers), haControlSize)
	}

	sshKey, err := p.ensureSSHKey(ctx, name, env)
	if err != nil {
		return err
	}
	hostPriv, hostPub, err := p.hostKeyMaterial(ctx, name, env)
	if err != nil {
		return err
	}
	network, err := p.ensureNetwork(ctx, name, env, haLocations())
	if err != nil {
		return err
	}
	image, err := p.resolveWorkerImage(ctx, "latest")
	if err != nil {
		return err
	}

	// Reconstruct the haControlSpec from current state so createHAServer
	// emits joiner user-data with the right pre-seeded token + tailnet
	// info if applicable.
	token, _ := p.store.Read(ctx, secrets.LocalKey(name, env, tokenSecretKey))
	endpoint, _ := p.store.Read(ctx, secrets.LocalKey(name, env, clusterEndpointKey))
	clusterEndpoint := strings.TrimPrefix(strings.TrimSuffix(endpoint, ":6443"), "https://")

	// Tailnet inference: if any server has a tailscale0 interface, this is a
	// tailnet cluster. We detect by trying to read tailscale ip on a survivor.
	tailnetCfg, err := p.detectTailnetConfig(ctx, name, env, servers[0])
	if err != nil {
		return fmt.Errorf("detect tailnet config: %w", err)
	}

	// LB inference: try to find one tagged for this cluster.
	var lbInf *lbInfra
	if lbs, _ := p.client.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "control-lb")},
	}); len(lbs) > 0 {
		lbInf = &lbInfra{LB: lbs[0], PublicIP: lbs[0].PublicNet.IPv4.IP.String()}
		if len(lbs[0].PrivateNet) > 0 {
			lbInf.PrivateIP = lbs[0].PrivateNet[0].IP.String()
		}
	}

	spec := haControlSpec{
		Name: name, Env: env,
		Locations:              haLocations(),
		Network:                network,
		Firewall:               nil, // re-applied per server below
		LB:                     lbInf,
		SSHKey:                 sshKey,
		K3sVersion:             defaultK3sVersion,
		TailnetURL:             tailnetCfg.URL,
		TailnetAuthCred:        tailnetCfg.AuthCred,
		TailnetTag:             tailnetCfg.Tag,
		HostKeyPublic:          strings.TrimSpace(hostPub),
		HostKeyPrivateIndented: indentForCloudConfig(hostPriv, 4),
		ClusterEndpoint:        clusterEndpoint,
		Image:                  image,
	}

	fw, _ := p.client.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "firewall")},
	})
	if len(fw) > 0 {
		spec.Firewall = fw[0]
	}

	for i, doomed := range servers {
		// Pick a survivor to point the joiner at.
		survivor := pickSurvivor(servers, i)
		leaderPrivateIP := firstPrivateIP(survivor)
		var leaderTailnetIP string
		if spec.tailnetMode() {
			leaderTailnetIP, err = p.readTailnetIP(ctx, name, env, firstPublicIP(survivor))
			if err != nil {
				return fmt.Errorf("read survivor tailnet IP: %w", err)
			}
		}

		if _, _, err := p.client.Server.DeleteWithResult(ctx, doomed); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete control plane %d: %w", doomed.ID, err)
		}
		// Wait for etcd to notice the dead member and clear it from quorum.
		// k3s defaults to ~30s for member liveness; double that for safety.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Second):
		}

		newSrv, err := p.createHAServer(ctx, spec, i+1, token, leaderPrivateIP, leaderTailnetIP)
		if err != nil {
			return fmt.Errorf("create replacement for index %d: %w", i+1, err)
		}
		if err := waitForSSH(ctx, firstPublicIP(newSrv), sshReadyTimeout); err != nil {
			return err
		}
		if err := p.waitForHAServerReady(ctx, name, env, firstPublicIP(newSrv)); err != nil {
			return err
		}
		if spec.Firewall != nil {
			if err := p.applyFirewall(ctx, spec.Firewall, newSrv); err != nil {
				return fmt.Errorf("apply firewall to replacement: %w", err)
			}
		}
		if spec.LB != nil {
			if err := p.attachServerToLB(ctx, spec.LB.LB, newSrv); err != nil {
				return fmt.Errorf("attach replacement to LB: %w", err)
			}
		}
	}
	return nil
}

type tailnetConfig struct {
	URL, AuthCred, Tag string
}

// detectTailnetConfig SSHes a survivor to see if tailscale is running. If so,
// we extract enough info to bring up a replacement node on the same tailnet.
//
// Limitation: we can recover the URL and tag from `tailscale status --json`
// but NOT the auth credential (tailscaled never exposes it back). For v1, if
// the original cluster used tailnet mode, rotate-control errors and asks the
// operator to re-run grow with the same --tailnet-key-file. Future PR can
// cache the tailnet cred in FileSecretStore at provision time.
func (p *Provider) detectTailnetConfig(ctx context.Context, name, env string, srv *hcloud.Server) (tailnetConfig, error) {
	client, err := p.sshClient(ctx, name, env, firstPublicIP(srv))
	if err != nil {
		return tailnetConfig{}, err
	}
	defer client.Close()
	out, err := runSSH(client, "command -v tailscale && tailscale status --peers=false --self=false 2>/dev/null | head -1 || true")
	if err != nil {
		return tailnetConfig{}, nil // not on tailnet
	}
	if strings.TrimSpace(out) == "" {
		return tailnetConfig{}, nil
	}
	return tailnetConfig{}, fmt.Errorf("HA rotate-control on a tailnet cluster requires re-running `bonsai grow` with the same --tailnet-key-file (tailscaled does not expose the auth credential back to the operator); v1 limitation")
}

// pickSurvivor returns the first server other than the one at index i.
func pickSurvivor(servers []*hcloud.Server, doomedIdx int) *hcloud.Server {
	for i, s := range servers {
		if i != doomedIdx {
			return s
		}
	}
	return nil
}
