package cli

import (
	"context"
	"fmt"

	"github.com/badri/bonsai/internal/config"
	"github.com/badri/bonsai/internal/provider"
	awsprov "github.com/badri/bonsai/internal/provider/aws"
	hetznerprov "github.com/badri/bonsai/internal/provider/hetzner"
	"github.com/spf13/cobra"
)

func newGrowCommand() *cobra.Command {
	var cfg config.ClusterConfig
	cmd := &cobra.Command{
		Use:   "grow",
		Short: "Provision or reconcile a cluster",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := selectProvider(cmd.Context(), cfg.Provider)
			if err != nil {
				return err
			}
			out, err := p.Provision(cmd.Context(), cfg)
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
	cmd.Flags().StringVar(&cfg.Provider, "provider", "aws", "aws | hetzner | digitalocean")
	cmd.Flags().StringVar(&cfg.Name, "name", "", "app name (required)")
	cmd.Flags().StringVar(&cfg.Env, "env", "dev", "environment")
	cmd.Flags().IntVar(&cfg.Workers, "workers", 1, "initial worker count")
	cmd.Flags().StringVar(&cfg.Region, "region", "us-east-1", "cloud region")
	cmd.Flags().BoolVar(&cfg.HAControl, "ha-control", false, "3-node embedded-etcd control plane across multiple AZs (~$60/mo extra)")
	cmd.Flags().StringVar(&cfg.TailnetURL, "tailnet-url", "", "headscale/tailscale login server URL — nodes join your tailnet on boot, no NLB/admin-CIDR (requires --tailnet-key-ssm)")
	cmd.Flags().StringVar(&cfg.TailnetKeySSMPath, "tailnet-key-ssm", "", "SSM path holding an OAuth client secret (tskey-client-..., recommended) or reusable pre-auth key (tskey-auth-...)")
	cmd.Flags().StringVar(&cfg.TailnetTag, "tailnet-tag", "tag:bonsai", "device tag nodes advertise (must be defined in your tailnet ACL; used with OAuth client secrets)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func selectProvider(ctx context.Context, name string) (provider.PlatformProvider, error) {
	switch name {
	case "aws":
		return awsprov.New(ctx)
	case "hetzner":
		return hetznerprov.New(ctx)
	default:
		return nil, fmt.Errorf("unknown provider %q (valid: aws, hetzner)", name)
	}
}
