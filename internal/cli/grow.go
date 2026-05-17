package cli

import (
	"context"
	"fmt"

	"github.com/badri/bonsai/internal/config"
	"github.com/badri/bonsai/internal/provider"
	awsprov "github.com/badri/bonsai/internal/provider/aws"
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
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func selectProvider(ctx context.Context, name string) (provider.PlatformProvider, error) {
	switch name {
	case "aws":
		return awsprov.New(ctx)
	default:
		return nil, fmt.Errorf("provider %q not implemented in Phase 1", name)
	}
}
