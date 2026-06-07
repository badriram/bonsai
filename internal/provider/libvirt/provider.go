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
	"runtime"
	"strings"
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
	defaultImageSHA256 = "" // empty = trust HTTPS for V1; fill in once we pin a version
	defaultNetwork     = "default"
	defaultMemoryMB    = 2048
	defaultVCPUs       = 2

	// 10 min — Alpine first boot + cloud-init + cgroups + apk add + k3s
	// install + sshd start takes 5–6 min on macOS HVF; doubling that for
	// slower hosts and image-pull latency.
	sshReadyTimeout = 10 * time.Minute
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
	// Default NAT network must exist and be active before VM-create. This is
	// the load-bearing one-time setup step on macOS and on freshly-installed
	// Linux libvirt — fail with the fix-it command rather than a virsh error
	// 10 minutes into provisioning.
	if err := ensureDefaultNetworkActive(ctx, uri); err != nil {
		return nil, err
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

	// In tailnet mode the auth cred (OAuth client secret or pre-auth key) is
	// baked into the first-boot script so each node can `tailscale up` and
	// join the operator's tailnet before k3s starts. File-based only on
	// libvirt — there's no managed parameter store.
	var tailnetCred string
	if cfg.TailnetMode() {
		raw, err := os.ReadFile(cfg.TailnetKeyFile)
		if err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("read tailnet auth_key_file %s: %w", cfg.TailnetKeyFile, err)
		}
		tailnetCred = strings.TrimSpace(string(raw))
		if tailnetCred == "" {
			return provider.PlatformOutputs{}, fmt.Errorf("tailnet auth_key_file %s is empty", cfg.TailnetKeyFile)
		}
	}

	progress.Step("provisioning control plane VM")
	control, vmIP, err := p.createControlVM(ctx, cfg, baseImage, tailnetCred, sshKey)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("control VM: %w", err)
	}

	// In tailnet mode, the VM is reachable on the vmnet IP only briefly —
	// once `tailscale up --accept-routes` runs and the VM's default route
	// flips to tailscale0, replies from the guest stop coming back through
	// the vmnet bridge. SSH (and everything else) needs to happen on the
	// tailnet IP from the start. We discover it from the host's tailscaled
	// rather than trying to race the VM's routing transition.
	sshIP := vmIP
	controlIP := vmIP
	if cfg.TailnetMode() {
		hostname := fmt.Sprintf("bonsai-%s-%s-control", cfg.Name, cfg.Env)
		progress.Step("waiting for %s to register with the operator's tailnet", hostname)
		tip, err := lookupTailnetIP(ctx, hostname, k3sReadyTimeout)
		if err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("tailnet discovery: %w", err)
		}
		progress.Step("control VM joined tailnet at %s — waiting for k3s", tip)
		sshIP = tip
		controlIP = tip
	} else {
		progress.Step("control VM ready at %s — waiting for k3s", vmIP)
	}

	if err := p.waitForReady(ctx, sshIP, sshKey, k3sReadyTimeout); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("waitForReady: %w", err)
	}

	token, kubeconfig, err := p.retrieveControlState(ctx, sshIP, sshKey, controlIP)
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
	if err := p.ensureWorkers(ctx, cfg, baseImage, tailnetCred, sshKey, controlIP, token, workerCount); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("workers: %w", err)
	}

	progress.Step("running in-cluster bootstrap (helm: cnpg, valkey, kured, suc)")
	bootstrapKubeconfig := kubeconfig
	if runtime.GOOS == "darwin" && !cfg.TailnetMode() {
		// macOS's kernel rejects connect() from non-Apple-signed binaries to
		// vmnet-bridged guest IPs. Bonsai's embedded helm + client-go would
		// fail dialing the k3s API at the guest IP. Open a loopback tunnel
		// (Apple-signed ssh sets it up; loopback connects are unrestricted)
		// and rewrite the bootstrap-time kubeconfig to point at it. The
		// kubeconfig saved to BONSAI_DATA_DIR keeps the guest IP unchanged.
		// Tailnet mode skips this — the tailnet IP routes through utun, which
		// is unrestricted, so Go's net.Dial works directly.
		cleanup, tunneledKC, err := tunnelKubeconfigDarwin(ctx, vmIP, sshKey, kubeconfig)
		if err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("bootstrap tunnel: %w", err)
		}
		defer cleanup()
		bootstrapKubeconfig = tunneledKC
	}
	out, err := cluster.Bootstrap(ctx, cluster.Config{
		Kubeconfig:         []byte(bootstrapKubeconfig),
		Name:               cfg.Name,
		Env:                cfg.Env,
		PostgresVolumeSize: cfg.PostgresVolumeSize,
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
