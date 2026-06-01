package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/badriram/bonsai/internal/state"
)

func newStatusCommand() *cobra.Command {
	var name, env, providerName string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// state.json first (fast, no cloud call). Gives us the declared
			// config + last-known endpoint + resource IDs without an API hop.
			if st := readStateForCluster(name, env); st != nil {
				fmt.Printf("cluster:           %s/%s\n", name, env)
				fmt.Printf("provisioned_at:    %s\n", st.ProvisionedAt.Format("2006-01-02 15:04:05 MST"))
				fmt.Printf("updated_at:        %s\n", st.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
				fmt.Printf("bonsai_version:    %s\n", st.BonsaiVersion)
				fmt.Printf("cluster_endpoint:  %s\n", st.ClusterEndpoint)
				fmt.Printf("declared:          provider=%s workers=%d ha=%v tailnet=%v\n",
					st.Declared.Provider, st.Declared.Workers, st.Declared.HAControl, st.Declared.TailnetMode())
				if st.Hetzner != nil && len(st.Hetzner.Servers) > 0 {
					fmt.Println("hetzner_servers:")
					for _, srv := range st.Hetzner.Servers {
						fmt.Printf("  - %-30s %-13s %-5s %-7s priv=%s\n",
							srv.Name, srv.Role, srv.Location, srv.ServerType, srv.PrivateIP)
					}
				}
				fmt.Println()
			}

			p, err := selectProvider(cmd.Context(), providerName)
			if err != nil {
				return err
			}
			s, err := p.Status(cmd.Context(), name, env)
			if err != nil {
				return err
			}
			fmt.Printf("live:              healthy=%v workers=%d k3s=%s ami=%s\n",
				s.Healthy, s.WorkerCount, s.K3sVersion, s.AMIID)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "aws", "")
	cmd.Flags().StringVar(&name, "name", "", "")
	cmd.Flags().StringVar(&env, "env", "dev", "")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func readStateForCluster(name, env string) *state.State {
	dataDir := os.Getenv("BONSAI_DATA_DIR")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dataDir = filepath.Join(home, ".bonsai")
	}
	st, _ := state.Read(state.Path(dataDir, name, env))
	return st
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
