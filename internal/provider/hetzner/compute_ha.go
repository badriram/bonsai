package hetzner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/badriram/bonsai/internal/progress"
)

// HA control plane orchestration on Hetzner.
//
// Imperative leader-first pattern (no SSM-lock equivalent here — Bonsai is
// the orchestrator):
//
//   1. Pre-seed a cluster-secret token in FileSecretStore.
//   2. Create leader in locations[0] with --cluster-init user-data.
//   3. Wait for SSH + readiness marker.
//   4. Learn the leader's reachable IP: private IP (LB mode) or tailnet IP
//      (tailnet mode, read via SSH `tailscale ip -4`).
//   5. Create joiners in locations[1] + locations[2] with --server pointing
//      at the leader IP, same pre-seeded token baked into user-data.
//   6. Apply the Hetzner Firewall to all 3 servers (idempotent on re-apply).
//   7. LB mode: attach all 3 to the LB target group via private IPs.

const haControlSize = 3

type haControlSpec struct {
	Name, Env  string
	Locations  []string         // HA spread; default haLocations()
	Network    *hcloud.Network  // private network for the cluster
	Firewall   *hcloud.Firewall // applied to all control plane servers
	LB         *lbInfra         // nil in tailnet mode
	SSHKey     *hcloud.SSHKey
	K3sVersion string

	// Tailnet config — empty in LB mode.
	TailnetURL      string
	TailnetAuthCred string
	TailnetTag      string

	// Host key + endpoint info.
	HostKeyPublic          string
	HostKeyPrivateIndented string
	ClusterEndpoint        string // LB public IP / tailnet IP — for --tls-san on the leader

	// Image to launch from (image-builder snapshot or stock Ubuntu).
	Image *hcloud.Image
}

func (s haControlSpec) tailnetMode() bool {
	return s.TailnetURL != "" && s.TailnetAuthCred != ""
}

// ensureControlPlaneHA returns (leader, allServers, leaderEndpointIP, err).
// leaderEndpointIP is the IP joiners + workers use as the cluster API endpoint
// when there's no LB (i.e. tailnet mode it's the leader's tailnet IP; LB mode
// it's the LB's public IP and this field is the leader's private IP for
// internal references).
func (p *Provider) ensureControlPlaneHA(ctx context.Context, spec haControlSpec) (leader *hcloud.Server, all []*hcloud.Server, leaderEndpointIP string, err error) {
	existing, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(spec.Name, spec.Env, "control-plane")},
	})
	if err != nil {
		return nil, nil, "", err
	}
	if len(existing) >= haControlSize {
		// Idempotent: the first server tagged control-plane is treated as the
		// leader for state-discovery purposes.
		first := existing[0]
		return first, existing, firstPrivateIP(first), nil
	}

	token, err := p.ensureHAToken(ctx, spec.Name, spec.Env)
	if err != nil {
		return nil, nil, "", err
	}

	progress.Step("creating leader in %s", spec.Locations[0])
	leader, err = p.createHAServer(ctx, spec, 1, token, "", "")
	if err != nil {
		return nil, nil, "", fmt.Errorf("create leader: %w", err)
	}
	progress.Step("leader created: %s (public %s) — waiting for SSH", leader.Name, firstPublicIP(leader))
	if err := waitForSSH(ctx, firstPublicIP(leader), sshReadyTimeout); err != nil {
		return nil, nil, "", err
	}
	progress.Step("leader SSH up — waiting for k3s ready marker (up to %s)", k3sReadyTimeout)
	if err := p.waitForHAServerReady(ctx, spec.Name, spec.Env, firstPublicIP(leader)); err != nil {
		return nil, nil, "", err
	}
	progress.Step("leader k3s ready")

	leaderPrivateIP := firstPrivateIP(leader)
	leaderTailnetIP := ""
	if spec.tailnetMode() {
		leaderTailnetIP, err = p.readTailnetIP(ctx, spec.Name, spec.Env, firstPublicIP(leader))
		if err != nil {
			return nil, nil, "", fmt.Errorf("read leader tailnet IP: %w", err)
		}
	}

	if err := p.applyFirewall(ctx, spec.Firewall, leader); err != nil {
		return nil, nil, "", fmt.Errorf("apply firewall to leader: %w", err)
	}

	all = append(all, leader)
	for i := 1; i < haControlSize; i++ {
		progress.Step("creating joiner-%d in %s", i+1, spec.Locations[i])
		joiner, err := p.createHAServer(ctx, spec, i+1, token, leaderPrivateIP, leaderTailnetIP)
		if err != nil {
			return nil, nil, "", fmt.Errorf("create joiner %d: %w", i+1, err)
		}
		progress.Step("joiner-%d created: %s — waiting for SSH", i+1, firstPublicIP(joiner))
		if err := waitForSSH(ctx, firstPublicIP(joiner), sshReadyTimeout); err != nil {
			return nil, nil, "", err
		}
		progress.Step("joiner-%d SSH up — waiting for k3s join", i+1)
		if err := p.waitForHAServerReady(ctx, spec.Name, spec.Env, firstPublicIP(joiner)); err != nil {
			return nil, nil, "", err
		}
		progress.Step("joiner-%d k3s joined", i+1)
		if err := p.applyFirewall(ctx, spec.Firewall, joiner); err != nil {
			return nil, nil, "", fmt.Errorf("apply firewall to joiner %d: %w", i+1, err)
		}
		all = append(all, joiner)
	}

	if spec.LB != nil {
		progress.Step("attaching %d control nodes to LB", len(all))
		for _, srv := range all {
			if err := p.attachServerToLB(ctx, spec.LB.LB, srv); err != nil {
				return nil, nil, "", fmt.Errorf("attach %d to LB: %w", srv.ID, err)
			}
		}
	}

	if spec.tailnetMode() {
		return leader, all, leaderTailnetIP, nil
	}
	return leader, all, leaderPrivateIP, nil
}

// createHAServer renders the right user-data template for the spec and
// creates one Hetzner Server attached to the cluster Network.
func (p *Provider) createHAServer(ctx context.Context, spec haControlSpec, index int, token, leaderPrivateIP, leaderTailnetIP string) (*hcloud.Server, error) {
	isLeader := index == 1
	location := spec.Locations[index-1]

	var userData string
	var err error
	if spec.tailnetMode() {
		userData, err = renderServerTailnetHAUserData(serverTailnetHAVars{
			Name: spec.Name, Env: spec.Env, K3sVersion: spec.K3sVersion,
			Token:                  token,
			IsLeader:               isLeader,
			NodeIndex:              index,
			LeaderTailnetIP:        leaderTailnetIP,
			TailnetURL:             spec.TailnetURL,
			TailnetAuthCred:        spec.TailnetAuthCred,
			TailnetTag:             spec.TailnetTag,
			HostKeyPublic:          spec.HostKeyPublic,
			HostKeyPrivateIndented: spec.HostKeyPrivateIndented,
		})
	} else {
		userData, err = renderServerHAUserData(serverHAVars{
			Name: spec.Name, Env: spec.Env, K3sVersion: spec.K3sVersion,
			Token:                  token,
			IsLeader:               isLeader,
			NodePrivateIP:          "", // Hetzner assigns; --node-ip can be set post-boot if needed
			LeaderPrivateIP:        leaderPrivateIP,
			ClusterEndpoint:        spec.ClusterEndpoint,
			HostKeyPublic:          spec.HostKeyPublic,
			HostKeyPrivateIndented: spec.HostKeyPrivateIndented,
		})
	}
	if err != nil {
		return nil, err
	}

	res, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       fmt.Sprintf("bonsai-%s-%s-control-%d", spec.Name, spec.Env, index),
		ServerType: &hcloud.ServerType{Name: defaultServerType},
		Image:      spec.Image,
		Location:   &hcloud.Location{Name: location},
		SSHKeys:    []*hcloud.SSHKey{spec.SSHKey},
		UserData:   userData,
		Networks:   []*hcloud.Network{spec.Network},
		Labels:     clusterLabels(spec.Name, spec.Env, "control-plane"),
	})
	if err != nil {
		return nil, err
	}
	// Refetch so PrivateNet[] is populated for the caller.
	srv, _, err := p.client.Server.GetByID(ctx, res.Server.ID)
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// waitForHAServerReady polls for /var/lib/bonsai-server-ready, the marker
// dropped by both server-ha and server-tailnet-ha at end of runcmd.
func (p *Provider) waitForHAServerReady(ctx context.Context, name, env, ip string) error {
	client, err := p.sshClient(ctx, name, env, ip)
	if err != nil {
		return err
	}
	defer client.Close()
	deadline := time.Now().Add(k3sReadyTimeout)
	for time.Now().Before(deadline) {
		if _, err := runSSH(client, "test -f /var/lib/bonsai-server-ready"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("server on %s never reached ready marker within %s", ip, k3sReadyTimeout)
}

// readTailnetIP SSHes in and runs `tailscale ip -4` on the leader. Used to
// learn the IP joiners + workers should connect to.
func (p *Provider) readTailnetIP(ctx context.Context, name, env, ip string) (string, error) {
	client, err := p.sshClient(ctx, name, env, ip)
	if err != nil {
		return "", err
	}
	defer client.Close()
	out, err := runSSH(client, "tailscale ip -4")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ensureHAToken pre-seeds the cluster-secret token in FileSecretStore on first
// call. Same value goes into leader + joiner + worker user-data; k3s uses it
// as both the cluster secret and the agent join token.
func (p *Provider) ensureHAToken(ctx context.Context, name, env string) (string, error) {
	key := secretKey(name, env, tokenSecretKey)
	if existing, err := p.store.Read(ctx, key); err == nil && existing != "" {
		return existing, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	if err := p.store.Write(ctx, key, tok); err != nil {
		return "", err
	}
	return tok, nil
}

func firstPublicIP(s *hcloud.Server) string {
	if s.PublicNet.IPv4.IP != nil {
		return s.PublicNet.IPv4.IP.String()
	}
	return ""
}

func firstPrivateIP(s *hcloud.Server) string {
	if len(s.PrivateNet) > 0 && s.PrivateNet[0].IP != nil {
		return s.PrivateNet[0].IP.String()
	}
	return ""
}
