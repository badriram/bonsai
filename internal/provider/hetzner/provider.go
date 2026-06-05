package hetzner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/badriram/bonsai/internal/cluster"
	bcfg "github.com/badriram/bonsai/internal/config"
	"github.com/badriram/bonsai/internal/provider"
	"github.com/badriram/bonsai/internal/secrets"
	"github.com/badriram/bonsai/internal/state"
)

// k3sVersionOrDefault returns v if non-empty, else the pinned default.
func k3sVersionOrDefault(v string) string {
	if v != "" {
		return v
	}
	return defaultK3sVersion
}

// Hetzner pinning. Bumps are intentional commits.
const (
	defaultK3sVersion       = "v1.31.0+k3s1"
	defaultLocation         = "nbg1" // Nuremberg — cheap, well-connected
	defaultControlImage     = "ubuntu-24.04"
	defaultServerType       = "cpx22" // 2 vCPU AMD, 4GB, ~€5/mo (cx22 deprecated 2026; cax11 arm stock thin)
	sshReadyTimeout         = 5 * time.Minute
	k3sReadyTimeout         = 10 * time.Minute
	kubeconfigSecretKey     = "kubeconfig"
	tokenSecretKey          = "token"
	clusterEndpointKey      = "cluster_endpoint"
	postgresURLKey          = "postgres_url"
	kvURLKey                = "kv_url"
)

// Provider implements provider.PlatformProvider against Hetzner Cloud using
// hcloud-go directly. Cluster state lives in:
//   - Hetzner labels on every resource (lookup model, mirrors AWS tags)
//   - A local FileSecretStore at ~/.bonsai/<name>-<env>/ for kubeconfig,
//     token, SSH private key, and Bootstrap outputs. Hetzner has no managed
//     Parameter Store equivalent; Object Storage is beta-tier and not worth
//     the dependency for Phase 2.
type Provider struct {
	client  *hcloud.Client
	store   secrets.Store
	dataDir string // BONSAI_DATA_DIR root; state.json lives at dataDir/<name>-<env>/state.json
}

func New(ctx context.Context) (*Provider, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("HCLOUD_TOKEN not set")
	}
	dataDir := os.Getenv("BONSAI_DATA_DIR")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		dataDir = filepath.Join(home, ".bonsai")
	}
	return &Provider{
		client:  hcloud.NewClient(hcloud.WithToken(token)),
		store:   secrets.NewFile(dataDir),
		dataDir: dataDir,
	}, nil
}

// Provision is idempotent: every step looks up by label, creates if missing.
//
// Three modes, picked by flags:
//   - Single-node (default): SSH key → floating IP → 1 control plane → 1+ workers
//   - HA + LB: SSH key → network → firewall → LB → 3 control planes → workers via LB
//   - HA + tailnet (--tailnet-url + --tailnet-key-file): SSH key → network →
//     firewall → 3 control planes on tailnet → workers via leader's tailnet IP
//     (no LB, no public 6443 from cluster nodes)
func (p *Provider) Provision(ctx context.Context, cfg bcfg.ClusterConfig) (provider.PlatformOutputs, error) {
	if cfg.HAControl {
		return p.provisionHA(ctx, cfg)
	}

	location := defaultLocation
	if len(cfg.Locations) > 0 {
		location = cfg.Locations[0]
	} else if cfg.Region != "" {
		location = cfg.Region // Hetzner exposes "regions" as location codes (nbg1, fsn1, hel1, ash, hil)
	}
	if cfg.AdminCIDR != "" {
		_ = os.Setenv("BONSAI_ADMIN_CIDR", cfg.AdminCIDR)
	}

	sshKey, err := p.ensureSSHKey(ctx, cfg.Name, cfg.Env)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("ssh key: %w", err)
	}

	fip, err := p.ensureControlFloatingIP(ctx, cfg.Name, cfg.Env, location)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("floating IP: %w", err)
	}

	controlIP := fip.IP.String()
	control, err := p.ensureControlPlane(ctx, cfg, location, sshKey, controlIP)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("control plane: %w", err)
	}
	if err := p.assignFloatingIP(ctx, fip, control); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("assign floating IP: %w", err)
	}

	if err := waitForSSH(ctx, controlIP, sshReadyTimeout); err != nil {
		return provider.PlatformOutputs{}, err
	}

	token, kubeconfig, err := p.retrieveControlState(ctx, cfg.Name, cfg.Env, controlIP)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("retrieve control state: %w", err)
	}
	if err := p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, tokenSecretKey), token); err != nil {
		return provider.PlatformOutputs{}, err
	}
	if err := p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, kubeconfigSecretKey), kubeconfig); err != nil {
		return provider.PlatformOutputs{}, err
	}
	if err := p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, clusterEndpointKey), "https://"+controlIP+":6443"); err != nil {
		return provider.PlatformOutputs{}, err
	}

	workerCount := cfg.Workers
	if workerCount < 1 {
		workerCount = 1
	}
	if err := p.ensureWorkers(ctx, cfg, location, sshKey, controlIP, token, workerCount); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("workers: %w", err)
	}

	out, err := cluster.Bootstrap(ctx, cluster.Config{
		Kubeconfig: []byte(kubeconfig),
		Name:       cfg.Name,
		Env:        cfg.Env,
		// BackupBucket left empty — Hetzner has no managed S3. CNPG runs
		// without barmanObjectStore; configure external S3 (R2, Backblaze) as
		// a Phase 2.1 follow-up if backups are required.
	})
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("in-cluster bootstrap: %w", err)
	}

	_ = p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, postgresURLKey), out.PostgresURL)
	_ = p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, kvURLKey), out.KVURL)

	if err := p.writeStateSingle(ctx, cfg, controlIP, fip); err != nil {
		// Non-fatal — see writeStateHA comment.
		fmt.Fprintf(os.Stderr, "warning: state.json write failed: %v\n", err)
	}

	return provider.PlatformOutputs{
		ClusterEndpoint:    "https://" + controlIP + ":6443",
		KubeconfigLocation: "file://" + secrets.LocalKey(cfg.Name, cfg.Env, kubeconfigSecretKey),
		PostgresURL:        out.PostgresURL,
		KVURL:              out.KVURL,
	}, nil
}

// writeStateSingle snapshots the single-node Hetzner cluster's state.
// Re-queries servers by label rather than threading IDs through every
// helper. Same non-fatal posture as the HA path.
func (p *Provider) writeStateSingle(ctx context.Context, cfg bcfg.ClusterConfig, controlIP string, fip *hcloud.FloatingIP) error {
	servers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: clusterSelector(cfg.Name, cfg.Env)},
	})
	if err != nil {
		return err
	}
	hs := &state.HetznerState{
		FloatingIPID: fip.ID,
		K3sVersion:   k3sVersionOrDefault(cfg.K3sVersion),
	}
	for _, s := range servers {
		role := "worker"
		if r, ok := s.Labels["bonsai.role"]; ok {
			role = r
		}
		hs.Servers = append(hs.Servers, state.HetznerServer{
			ID:         s.ID,
			Name:       s.Name,
			Role:       role,
			Location:   s.Datacenter.Location.Name,
			ServerType: s.ServerType.Name,
			PublicIP:   firstPublicIP(s),
			PrivateIP:  firstPrivateIP(s),
		})
	}
	st := &state.State{
		BonsaiVersion:   versionInfo(),
		Declared:        cfg,
		ClusterEndpoint: "https://" + controlIP + ":6443",
		Hetzner:         hs,
	}
	if existing, _ := state.Read(state.Path(p.dataDir, cfg.Name, cfg.Env)); existing != nil {
		st.ProvisionedAt = existing.ProvisionedAt
	}
	return state.Write(state.Path(p.dataDir, cfg.Name, cfg.Env), st)
}

// retrieveControlState SSHs to the control plane after server.sh finishes and
// reads /etc/rancher/k3s/k3s.yaml + the cluster join token.
func (p *Provider) retrieveControlState(ctx context.Context, name, env, ip string) (token, kubeconfig string, err error) {
	client, err := p.sshClient(ctx, name, env, ip)
	if err != nil {
		return "", "", err
	}
	defer client.Close()

	// Wait for server.sh to drop its readiness marker — sshd is up well before k3s is.
	deadline := time.Now().Add(k3sReadyTimeout)
	for time.Now().Before(deadline) {
		if _, err := runSSH(client, "test -f /var/lib/bonsai-server-ready"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}

	tok, err := runSSH(client, "cat /var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return "", "", fmt.Errorf("read token: %w", err)
	}
	kc, err := runSSH(client, "cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return "", "", fmt.Errorf("read kubeconfig: %w", err)
	}
	return trimNewline(tok), rewriteKubeconfig(kc, ip), nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
