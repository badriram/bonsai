package cli

import "github.com/spf13/cobra"

func NewRootCommand() *cobra.Command {
	advanced := false
	root := &cobra.Command{
		Use:   "bonsai",
		Short: "Provision k3s clusters with Postgres + KV on any cloud",
		Long: `Bonsai is a self-service infrastructure CLI. One command provisions
a production k3s cluster with Postgres and KV included.

Developer verbs: grow, status, logs.
Operator verbs (--advanced): bake-ami, rotate-workers, rotate-control,
upgrade, destroy.`,
	}
	root.PersistentFlags().BoolVar(&advanced, "advanced", false, "show operator commands in help")

	root.AddCommand(
		newGrowCommand(),
		newStatusCommand(),
		newLogsCommand(),
	)

	// Operator commands — hidden unless --advanced is passed.
	for _, c := range []*cobra.Command{
		newBakeAMICommand(),
		newRotateWorkersCommand(),
		newRotateControlCommand(),
		newUpgradeCommand(),
		newDestroyCommand(),
	} {
		c.Hidden = !advanced
		root.AddCommand(c)
	}

	return root
}
