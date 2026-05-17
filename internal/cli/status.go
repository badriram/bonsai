package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCommand() *cobra.Command {
	var name, env, providerName string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := selectProvider(cmd.Context(), providerName)
			if err != nil {
				return err
			}
			s, err := p.Status(cmd.Context(), name, env)
			if err != nil {
				return err
			}
			fmt.Printf("cluster: %s/%s\nhealthy: %v\nworkers: %d\nk3s:     %s\nAMI:     %s\n",
				name, env, s.Healthy, s.WorkerCount, s.K3sVersion, s.AMIID)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "aws", "")
	cmd.Flags().StringVar(&name, "name", "", "")
	cmd.Flags().StringVar(&env, "env", "dev", "")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newLogsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logs",
		Short: "Tail cluster or workload logs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("not implemented in Phase 1")
		},
	}
}
