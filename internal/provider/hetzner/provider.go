package hetzner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/badri/bonsai/internal/cluster"
	bcfg "github.com/badri/bonsai/internal/config"
	"github.com/badri/bonsai/internal/provider"
	"github.com/badri/bonsai/internal/secrets"
)

// Hetzner pinning. Bumps are intentional commits.
const (
	defaultK3sVersion       = "v1.31.0+k3s1"
	defaultLocation         = "nbg1" // Nuremberg — cheap, well-connected
	defaultControlImage     = "ubuntu-24.04"
	defaultServerType       = "cx22" // 2 vCPU, 4GB, ~€5/mo
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
	client *hcloud.Client
	store  secrets.Store
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
		client: hcloud.NewClient(hcloud.WithToken(token)),
		store:  secrets.NewFile(dataDir),
	}, nil
}

// Provision is idempotent: every step looks up by label, creates if missing.
// Order: SSH key → floating IP → control plane → wait → retrieve kubeconfig +
// token → workers → cluster.Bootstrap → save outputs.
func (p *Provider) Provision(ctx context.Context, cfg bcfg.ClusterConfig) (provider.PlatformOutputs, error) {
	location := defaultLocation
	if cfg.Region != "" {
		location = cfg.Region // Hetzner exposes "regions" as location codes (nbg1, fsn1, hel1, ash, hil)
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
	control, err := p.ensureControlPlane(ctx, cfg.Name, cfg.Env, location, sshKey, controlIP)
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
	if err := p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, tokenSecretKey), token); err != nil {
		return provider.PlatformOutputs{}, err
	}
	if err := p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, kubeconfigSecretKey), kubeconfig); err != nil {
		return provider.PlatformOutputs{}, err
	}
	if err := p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, clusterEndpointKey), "https://"+controlIP+":6443"); err != nil {
		return provider.PlatformOutputs{}, err
	}

	workerCount := cfg.Workers
	if workerCount < 1 {
		workerCount = 1
	}
	if err := p.ensureWorkers(ctx, cfg.Name, cfg.Env, location, sshKey, controlIP, token, workerCount); err != nil {
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

	_ = p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, postgresURLKey), out.PostgresURL)
	_ = p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, kvURLKey), out.KVURL)

	return provider.PlatformOutputs{
		ClusterEndpoint:    "https://" + controlIP + ":6443",
		KubeconfigLocation: "file://" + secretKey(cfg.Name, cfg.Env, kubeconfigSecretKey),
		PostgresURL:        out.PostgresURL,
		KVURL:              out.KVURL,
	}, nil
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
