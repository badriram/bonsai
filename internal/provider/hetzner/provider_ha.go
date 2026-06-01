package hetzner

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/badriram/bonsai/internal/cluster"
	bcfg "github.com/badriram/bonsai/internal/config"
	"github.com/badriram/bonsai/internal/progress"
	"github.com/badriram/bonsai/internal/provider"
)

// provisionHA handles the --ha-control path on Hetzner. Two sub-modes:
//   - tailnet (--tailnet-key-file set): no LB, cluster API on tailnet IPs
//   - LB     (default with --ha-control):     Hetzner Load Balancer fronts 6443
func (p *Provider) provisionHA(ctx context.Context, cfg bcfg.ClusterConfig) (provider.PlatformOutputs, error) {
	locations := haLocations() // nbg1, fsn1, hel1
	progress.Step("hetzner HA grow: cluster=%s env=%s locations=%v workers=%d tailnet=%v",
		cfg.Name, cfg.Env, locations, cfg.Workers, cfg.TailnetMode())

	progress.Step("ensuring SSH + host keys")
	sshKey, err := p.ensureSSHKey(ctx, cfg.Name, cfg.Env)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("ssh key: %w", err)
	}
	hostPriv, hostPub, err := p.hostKeyMaterial(ctx, cfg.Name, cfg.Env)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}

	progress.Step("ensuring network 10.0.0.0/16 (subnets per location)")
	network, err := p.ensureNetwork(ctx, cfg.Name, cfg.Env, locations)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("network: %w", err)
	}

	image, err := p.resolveWorkerImage(ctx, "latest")
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("resolve image: %w", err)
	}

	var (
		lbInfra         *lbInfra
		clusterEndpoint string // for --tls-san on the leader + the public output
		workerJoinIP    string // what worker user-data uses for K3S_URL
		lbPrivateIPForFW string // empty in tailnet mode
	)

	tailnetMode := cfg.TailnetMode()
	tailnetCred := ""
	if tailnetMode {
		tailnetCred, err = p.readTailnetCred(cfg)
		if err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("read tailnet credential: %w", err)
		}
	} else {
		progress.Step("ensuring load balancer (lb11)")
		lbInfra, err = p.ensureLoadBalancer(ctx, cfg.Name, cfg.Env, network)
		if err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("load balancer: %w", err)
		}
		progress.Step("LB ready: public=%s private=%s", lbInfra.PublicIP, lbInfra.PrivateIP)
		clusterEndpoint = lbInfra.PublicIP
		workerJoinIP = lbInfra.PublicIP
		lbPrivateIPForFW = lbInfra.PrivateIP
	}

	progress.Step("ensuring firewall")
	fw, err := p.ensureFirewall(ctx, cfg.Name, cfg.Env, lbPrivateIPForFW)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("firewall: %w", err)
	}

	spec := haControlSpec{
		Name: cfg.Name, Env: cfg.Env,
		Locations:              locations,
		Network:                network,
		Firewall:               fw,
		LB:                     lbInfra,
		SSHKey:                 sshKey,
		K3sVersion:             defaultK3sVersion,
		TailnetURL:             cfg.TailnetURL,
		TailnetAuthCred:        tailnetCred,
		TailnetTag:             cfg.TailnetTag,
		HostKeyPublic:          strings.TrimSpace(hostPub),
		HostKeyPrivateIndented: indentForCloudConfig(hostPriv, 4),
		ClusterEndpoint:        clusterEndpoint,
		Image:                  image,
	}
	progress.Step("provisioning 3-node control plane (leader-first)")
	leader, _, leaderReachableIP, err := p.ensureControlPlaneHA(ctx, spec)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("control plane HA: %w", err)
	}
	progress.Step("control plane ready: leader reachable at %s", leaderReachableIP)

	if tailnetMode {
		clusterEndpoint = leaderReachableIP // tailnet IP
		workerJoinIP = leaderReachableIP
	}

	// Pull kubeconfig from the leader. SSH IP depends on mode: in LB mode
	// the firewall opens admin_cidr → public 22; in tailnet mode public 22
	// is closed and we route via the leader's tailnet IP (operator must be
	// on the tailnet, which they are by definition — they hold the cred).
	sshIP := firstPublicIP(leader)
	if tailnetMode {
		sshIP = leaderReachableIP
	}
	kubeconfig, err := p.fetchKubeconfig(ctx, cfg.Name, cfg.Env, sshIP)
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("fetch kubeconfig: %w", err)
	}
	kubeconfig = rewriteKubeconfig(kubeconfig, clusterEndpoint)
	token, _ := p.store.Read(ctx, secretKey(cfg.Name, cfg.Env, tokenSecretKey)) // pre-seeded in ensureHAToken

	if err := p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, kubeconfigSecretKey), kubeconfig); err != nil {
		return provider.PlatformOutputs{}, err
	}
	if err := p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, clusterEndpointKey), "https://"+clusterEndpoint+":6443"); err != nil {
		return provider.PlatformOutputs{}, err
	}

	workerCount := cfg.Workers
	if workerCount < 1 {
		workerCount = 1
	}
	progress.Step("provisioning %d worker(s)", workerCount)
	if err := p.ensureWorkersHA(ctx, cfg.Name, cfg.Env, locations, sshKey, network, fw, workerJoinIP, token, workerCount, tailnetMode, tailnetCred, cfg); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("workers: %w", err)
	}
	progress.Step("workers ready")

	progress.Step("running in-cluster bootstrap (helm: cnpg, valkey, kured, suc)")
	out, err := cluster.Bootstrap(ctx, cluster.Config{
		Kubeconfig: []byte(kubeconfig),
		Name:       cfg.Name,
		Env:        cfg.Env,
	})
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("in-cluster bootstrap: %w", err)
	}

	_ = p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, postgresURLKey), out.PostgresURL)
	_ = p.store.Write(ctx, secretKey(cfg.Name, cfg.Env, kvURLKey), out.KVURL)

	return provider.PlatformOutputs{
		ClusterEndpoint:    "https://" + clusterEndpoint + ":6443",
		KubeconfigLocation: "file://" + secretKey(cfg.Name, cfg.Env, kubeconfigSecretKey),
		PostgresURL:        out.PostgresURL,
		KVURL:              out.KVURL,
	}, nil
}

// readTailnetCred reads the tailnet credential from disk. Bonsai loads it
// once at grow time and bakes it into cloud-init — the operator's file is
// the source of truth, the cluster instances never read it back at runtime
// (Hetzner has no SSM-equivalent for the boot script to pull from).
func (p *Provider) readTailnetCred(cfg bcfg.ClusterConfig) (string, error) {
	if cfg.TailnetKeyFile == "" {
		return "", fmt.Errorf("tailnet mode on Hetzner requires --tailnet-key-file")
	}
	b, err := os.ReadFile(cfg.TailnetKeyFile)
	if err != nil {
		return "", err
	}
	// Accept either a bare token or the Tailscale admin UI's two-line
	// "Client ID: ...\nClient secret: tskey-..." copy-paste. Scan tokens
	// for the first one matching tskey-{client,auth}-* and use it.
	// An embedded newline would otherwise break the rendered shell var
	// AND the cloud-init YAML block scalar that wraps it.
	for _, tok := range strings.Fields(string(b)) {
		if strings.HasPrefix(tok, "tskey-client-") || strings.HasPrefix(tok, "tskey-auth-") {
			return tok, nil
		}
	}
	return "", fmt.Errorf("no tskey-client-* or tskey-auth-* token found in %s", cfg.TailnetKeyFile)
}

// fetchKubeconfig SSHes the leader and reads /etc/rancher/k3s/k3s.yaml.
// Token comes from ensureHAToken (pre-seeded into FileSecretStore before
// the leader boots).
//
// sshIP is whichever address Bonsai can still reach after the cluster
// firewall is applied: the public IP (LB mode, with admin_cidr open) or
// the leader's tailnet IP (tailnet mode, where public 22 is closed).
func (p *Provider) fetchKubeconfig(ctx context.Context, name, env, sshIP string) (string, error) {
	client, err := p.sshClient(ctx, name, env, sshIP)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return runSSH(client, "cat /etc/rancher/k3s/k3s.yaml")
}

// ensureWorkersHA brings up `desired` workers, attached to the same Hetzner
// Network as the control plane, with the correct user-data variant for
// LB or tailnet mode.
func (p *Provider) ensureWorkersHA(
	ctx context.Context,
	name, env string,
	locations []string,
	sshKey *hcloud.SSHKey,
	network *hcloud.Network,
	fw *hcloud.Firewall,
	joinIP, token string,
	desired int,
	tailnetMode bool,
	tailnetCred string,
	cfg bcfg.ClusterConfig,
) error {
	current, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "worker")},
	})
	if err != nil {
		return err
	}
	if len(current) >= desired {
		return nil
	}
	image, err := p.resolveWorkerImage(ctx, "latest")
	if err != nil {
		return err
	}

	for i := len(current); i < desired; i++ {
		var userData string
		if tailnetMode {
			userData, err = renderWorkerTailnetUserData(workerTailnetVars{
				Name: name, Env: env, K3sVersion: defaultK3sVersion,
				Token:            token,
				NodeIndex:        i + 1,
				ControlTailnetIP: joinIP,
				TailnetURL:       cfg.TailnetURL,
				TailnetAuthCred:  tailnetCred,
				TailnetTag:       cfg.TailnetTag,
			})
		} else {
			userData, err = renderWorkerUserData(workerVars{
				ControlIP: joinIP, K3sVersion: defaultK3sVersion, Token: token,
			})
		}
		if err != nil {
			return err
		}
		res, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
			Name:       fmt.Sprintf("bonsai-%s-%s-worker-%d", name, env, i+1),
			ServerType: &hcloud.ServerType{Name: defaultServerType},
			Image:      image,
			Location:   &hcloud.Location{Name: locations[i%len(locations)]},
			SSHKeys:    []*hcloud.SSHKey{sshKey},
			UserData:   userData,
			Networks:   []*hcloud.Network{network},
			Labels:     clusterLabels(name, env, "worker"),
		})
		if err != nil {
			return fmt.Errorf("create worker %d: %w", i+1, err)
		}
		if err := p.applyFirewall(ctx, fw, res.Server); err != nil {
			return fmt.Errorf("apply firewall to worker %d: %w", i+1, err)
		}
	}
	return nil
}
