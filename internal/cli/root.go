package cli

import "github.com/spf13/cobra"

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "bonsai",
		Short: "Provision k3s clusters with Postgres + KV on any cloud",
		Long: `Bonsai is a self-service infrastructure CLI. One command provisions
a production k3s cluster with Postgres and KV included.

Developer verbs: grow, plan, status, logs.
Operator verbs (--advanced): bake-image, rotate-workers, rotate-control,
upgrade, destroy.`,
	}
	root.PersistentFlags().Bool("advanced", false, "show operator commands in help")

	root.AddCommand(
		newGrowCommand(),
		newPlanCommand(),
		newStatusCommand(),
		newLogsCommand(),
	)

	operator := []*cobra.Command{
		newBakeImageCommand(),
		newRotateWorkersCommand(),
		newRotateControlCommand(),
		newUpgradeCommand(),
		newDestroyCommand(),
	}
	for _, c := range operator {
		c.Hidden = true
		root.AddCommand(c)
	}

	// Cobra builds the command tree before parsing flags, so we flip Hidden
	// inside the help function — which runs after flag parsing.
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		advanced, _ := c.Root().PersistentFlags().GetBool("advanced")
		for _, o := range operator {
			o.Hidden = !advanced
		}
		defaultHelp(c, args)
	})

	return root
}
