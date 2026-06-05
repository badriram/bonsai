// Package libvirt implements provider.PlatformProvider against a libvirt
// host using virsh + qemu-img + genisoimage as subprocesses. No cgo, no
// libvirt-go binding. Operator's host must have libvirt-daemon running and
// the three CLIs on PATH.
//
// Design choices that follow from Alpine + on-prem:
//   - No LB, no firewall: libvirt's default NAT is enough for V1, operator
//     hits any control plane IP directly.
//   - No cloud-init YAML wrapper: first-boot is a raw POSIX shell script
//     delivered via NoCloud config drive ISO. The same Alpine cloud image
//     ships cloud-init; we use only its scripts-user module to exec our
//     shebang script. Structural avoidance of bug-#11 class.
//   - Image cache: upstream Alpine qcow2 downloaded once to
//     BONSAI_DATA_DIR/_images/, then each VM gets a qcow2 backing-file
//     overlay so spin-up is seconds and disk usage stays bounded.
package libvirt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/badriram/bonsai/internal/cluster"
	bcfg "github.com/badriram/bonsai/internal/config"
	"github.com/badriram/bonsai/internal/progress"
	"github.com/badriram/bonsai/internal/provider"
	"github.com/badriram/bonsai/internal/secrets"
	"github.com/badriram/bonsai/internal/state"
)

const (
	defaultURI         = "qemu:///system"
	defaultK3sVersion  = "v1.31.0+k3s1"
	defaultImageURL    = "https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/cloud/nocloud_alpine-3.20.3-x86_64-uefi-cloudinit-r0.qcow2"
	defaultImageSHA256 = "" // empty = trust HTTPS for V1; fill in once we pin a version
	defaultNetwork     = "default"
	defaultMemoryMB    = 2048
	defaultVCPUs       = 2
	defaultDiskGB      = 10

	sshReadyTimeout = 5 * time.Minute
	k3sReadyTimeout = 10 * time.Minute

	tokenSecretKey      = "token"
	kubeconfigSecretKey = "kubeconfig"
	clusterEndpointKey  = "cluster_endpoint"
	postgresURLKey      = "postgres_url"
	kvURLKey            = "kv_url"
	sshPrivateKeyKey    = "ssh_private_key"
)

type Provider struct {
	uri      string
	dataDir  string
	imageDir string
	store    secrets.Store
}

func New(ctx context.Context) (*Provider, error) {
	// Verify the three subprocesses we depend on are on PATH.
	for _, bin := range []string{"virsh", "qemu-img", binISO()} {
		if _, err := exec.LookPath(bin); err != nil {
			return nil, fmt.Errorf("libvirt provider needs %q on PATH: %w", bin, err)
		}
	}
	uri := os.Getenv("LIBVIRT_URI")
	if uri == "" {
		uri = defaultURI
	}
	dataDir := os.Getenv("BONSAI_DATA_DIR")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		dataDir = filepath.Join(home, ".bonsai")
	}
	imageDir := filepath.Join(dataDir, "_images")
	if err := os.MkdirAll(imageDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", imageDir, err)
	}
	// Smoke-test the libvirt connection up-front so misconfiguration is a
	// fast error instead of a delayed VM-create failure.
	if out, err := virsh(ctx, uri, "version"); err != nil {
		return nil, fmt.Errorf("virsh connect %s: %w (output: %s)", uri, err, string(out))
	}
	return &Provider{
		uri:      uri,
		dataDir:  dataDir,
		imageDir: imageDir,
		store:    secrets.NewFile(dataDir),
	}, nil
}

// Provision is V1: single-node only. HA path comes in a follow-up so this
// PR stays reviewable. cfg.HAControl is rejected with a clear message.
func (p *Provider) Provision(ctx context.Context, cfg bcfg.ClusterConfig) (provider.PlatformOutputs, error) {
	if cfg.HAControl {
		return provider.PlatformOutputs{}, fmt.Errorf("libvirt HA control plane lands in a follow-up PR; use ha_control: false for now")
	}
	progress.Step("libvirt grow: cluster=%s env=%s uri=%s workers=%d", cfg.Name, cfg.Env, p.uri, cfg.Workers)

	progress.Step("ensuring SSH key + Alpine base image")
	sshKey, err := p.ensureSSHKey(ctx, cfg.Name, cfg.Env)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}
	baseImage, err := p.ensureBaseImage(ctx)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}

	progress.Step("provisioning control plane VM")
	control, controlIP, err := p.createControlVM(ctx, cfg, baseImage, sshKey)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("control VM: %w", err)
	}
	progress.Step("control VM ready at %s — waiting for k3s", controlIP)

	if err := p.waitForReady(ctx, controlIP, sshKey, k3sReadyTimeout); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("waitForReady: %w", err)
	}

	token, kubeconfig, err := p.retrieveControlState(ctx, controlIP, sshKey)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("retrieve control state: %w", err)
	}
	if err := p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, tokenSecretKey), token); err != nil {
		return provider.PlatformOutputs{}, err
	}
	if err := p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, kubeconfigSecretKey), kubeconfig); err != nil {
		return provider.PlatformOutputs{}, err
	}
	clusterEndpoint := "https://" + controlIP + ":6443"
	if err := p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, clusterEndpointKey), clusterEndpoint); err != nil {
		return provider.PlatformOutputs{}, err
	}

	workerCount := cfg.Workers
	if workerCount < 1 {
		workerCount = 1
	}
	progress.Step("provisioning %d worker VM(s)", workerCount)
	if err := p.ensureWorkers(ctx, cfg, baseImage, sshKey, controlIP, token, workerCount); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("workers: %w", err)
	}

	progress.Step("running in-cluster bootstrap (helm: cnpg, valkey, kured, suc)")
	out, err := cluster.Bootstrap(ctx, cluster.Config{
		Kubeconfig: []byte(kubeconfig),
		Name:       cfg.Name,
		Env:        cfg.Env,
	})
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("in-cluster bootstrap: %w", err)
	}
	_ = p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, postgresURLKey), out.PostgresURL)
	_ = p.store.Write(ctx, secrets.LocalKey(cfg.Name, cfg.Env, kvURLKey), out.KVURL)

	if err := p.writeState(ctx, cfg, clusterEndpoint, controlIP, control); err != nil {
		fmt.Fprintf(os.Stderr, "warning: state.json write failed: %v\n", err)
	}
	return provider.PlatformOutputs{
		ClusterEndpoint:    clusterEndpoint,
		KubeconfigLocation: "file://" + secrets.LocalKey(cfg.Name, cfg.Env, kubeconfigSecretKey),
		PostgresURL:        out.PostgresURL,
		KVURL:              out.KVURL,
	}, nil
}

func (p *Provider) writeState(ctx context.Context, cfg bcfg.ClusterConfig, endpoint, controlIP string, controlUUID string) error {
	_ = ctx
	st := &state.State{
		BonsaiVersion:   "v0.2.2+libvirt",
		Declared:        cfg,
		ClusterEndpoint: endpoint,
		// Libvirt block is intentionally minimal in V1; richer detail
		// lands when state.AWSState gets fleshed out alongside.
	}
	if existing, _ := state.Read(state.Path(p.dataDir, cfg.Name, cfg.Env)); existing != nil {
		st.ProvisionedAt = existing.ProvisionedAt
	}
	return state.Write(state.Path(p.dataDir, cfg.Name, cfg.Env), st)
}

// Stubs for the operator-only interface methods. Implementing these on
// libvirt is a Phase-4-part-2 lift; V1 errors loudly so an operator who
// tries `bonsai rotate-control --provider libvirt` understands the gap.

func (p *Provider) RotateWorkers(ctx context.Context, name, env, amiRef string) error {
	return fmt.Errorf("libvirt RotateWorkers: not implemented in V1 (destroy + grow until task #34 lands the bake-image flow)")
}
func (p *Provider) UpgradeK3s(ctx context.Context, name, env, version string) error {
	return fmt.Errorf("libvirt UpgradeK3s: not implemented in V1 — apply the system-upgrade-controller Plan CRD by hand for now")
}
func (p *Provider) UpgradeComponent(ctx context.Context, name, env, component string) error {
	return fmt.Errorf("libvirt UpgradeComponent: not implemented in V1 — bump charts.go and helm-upgrade the release by hand")
}
func (p *Provider) BakeImage(ctx context.Context, k3sVersion string) (string, error) {
	return "", fmt.Errorf("libvirt BakeImage: lands in task #34 (Alpine bake-image: generalize the pattern)")
}
func (p *Provider) RotateControl(ctx context.Context, name, env string) error {
	return fmt.Errorf("libvirt RotateControl: not implemented in V1 — destroy + grow until snapshot-restore lands")
}
