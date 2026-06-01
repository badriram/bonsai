package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/badriram/bonsai/internal/config"
	"github.com/badriram/bonsai/internal/provider"
	awsprov "github.com/badriram/bonsai/internal/provider/aws"
	hetznerprov "github.com/badriram/bonsai/internal/provider/hetzner"
	libvirtprov "github.com/badriram/bonsai/internal/provider/libvirt"
	"github.com/spf13/cobra"
)

func newGrowCommand() *cobra.Command {
	var cfg config.ClusterConfig
	var configPath string
	cmd := &cobra.Command{
		Use:   "grow",
		Short: "Provision or reconcile a cluster",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveGrowConfig(cmd, cfg, configPath)
			if err != nil {
				return err
			}
			p, err := selectProvider(cmd.Context(), resolved.Provider)
			if err != nil {
				return err
			}
			out, err := p.Provision(cmd.Context(), resolved)
			if err != nil {
				return err
			}
			fmt.Printf("CLUSTER_ENDPOINT  %s\n", out.ClusterEndpoint)
			fmt.Printf("KUBECONFIG        %s\n", out.KubeconfigLocation)
			fmt.Printf("POSTGRES_URL      %s\n", out.PostgresURL)
			fmt.Printf("KV_URL            %s\n", out.KVURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to bonsai.yaml (auto-discovers ./bonsai.yaml if unset)")
	cmd.Flags().StringVar(&cfg.Provider, "provider", "aws", "aws | hetzner | digitalocean")
	cmd.Flags().StringVar(&cfg.Name, "name", "", "app name (required if no config file)")
	cmd.Flags().StringVar(&cfg.Env, "env", "dev", "environment")
	cmd.Flags().IntVar(&cfg.Workers, "workers", 1, "initial worker count")
	cmd.Flags().StringVar(&cfg.Region, "region", "us-east-1", "cloud region")
	cmd.Flags().BoolVar(&cfg.HAControl, "ha-control", false, "3-node embedded-etcd control plane across multiple AZs (~$60/mo extra)")
	cmd.Flags().StringVar(&cfg.AdminCIDR, "admin-cidr", "", "operator source CIDR for SSH + 6443 (overrides BONSAI_ADMIN_CIDR; required unless tailnet)")
	cmd.Flags().StringVar(&cfg.TailnetURL, "tailnet-url", "", "headscale/tailscale login server URL — nodes join your tailnet on boot, no NLB/admin-CIDR")
	cmd.Flags().StringVar(&cfg.TailnetKeySSMPath, "tailnet-key-ssm", "", "AWS only: SSM path holding an OAuth client secret (tskey-client-...) or reusable pre-auth key (tskey-auth-...)")
	cmd.Flags().StringVar(&cfg.TailnetKeyFile, "tailnet-key-file", "", "Hetzner only: local filesystem path to a file holding the tailnet credential")
	cmd.Flags().StringVar(&cfg.TailnetTag, "tailnet-tag", "tag:bonsai", "device tag nodes advertise (must be defined in your tailnet ACL)")
	return cmd
}

// resolveGrowConfig merges --config (if set or auto-discovered) with CLI
// flags. Flags that were explicitly set on the command line override file
// values; defaulted flags do not. Final config is validated before return.
func resolveGrowConfig(cmd *cobra.Command, flagsCfg config.ClusterConfig, configPath string) (config.ClusterConfig, error) {
	if configPath == "" {
		if _, err := os.Stat("bonsai.yaml"); err == nil {
			configPath = "bonsai.yaml"
		}
	}

	resolved := flagsCfg
	if configPath != "" {
		loaded, err := config.Load(configPath)
		if err != nil {
			return resolved, err
		}
		resolved = loaded
		// CLI flags that were explicitly set override the file. Cobra's
		// Flag.Changed tells us which flags came from the command line vs
		// from their defaults.
		applyFlagOverrides(cmd, flagsCfg, &resolved)
	}

	if resolved.Name == "" {
		return resolved, fmt.Errorf("--name (or name: in bonsai.yaml) is required")
	}
	if err := config.Validate(resolved); err != nil {
		return resolved, err
	}
	return resolved, nil
}

func applyFlagOverrides(cmd *cobra.Command, fromFlags config.ClusterConfig, into *config.ClusterConfig) {
	if cmd.Flag("provider").Changed {
		into.Provider = fromFlags.Provider
	}
	if cmd.Flag("name").Changed {
		into.Name = fromFlags.Name
	}
	if cmd.Flag("env").Changed {
		into.Env = fromFlags.Env
	}
	if cmd.Flag("workers").Changed {
		into.Workers = fromFlags.Workers
	}
	if cmd.Flag("region").Changed {
		into.Region = fromFlags.Region
	}
	if cmd.Flag("ha-control").Changed {
		into.HAControl = fromFlags.HAControl
	}
	if cmd.Flag("admin-cidr").Changed {
		into.AdminCIDR = fromFlags.AdminCIDR
	}
	if cmd.Flag("tailnet-url").Changed {
		into.TailnetURL = fromFlags.TailnetURL
	}
	if cmd.Flag("tailnet-key-ssm").Changed {
		into.TailnetKeySSMPath = fromFlags.TailnetKeySSMPath
	}
	if cmd.Flag("tailnet-key-file").Changed {
		into.TailnetKeyFile = fromFlags.TailnetKeyFile
	}
	if cmd.Flag("tailnet-tag").Changed {
		into.TailnetTag = fromFlags.TailnetTag
	}
}

func selectProvider(ctx context.Context, name string) (provider.PlatformProvider, error) {
	switch name {
	case "aws":
		return awsprov.New(ctx)
	case "hetzner":
		return hetznerprov.New(ctx)
	case "libvirt":
		return libvirtprov.New(ctx)
	default:
		return nil, fmt.Errorf("unknown provider %q (valid: aws, hetzner, libvirt)", name)
	}
}
