package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Operator-surface commands. Hidden from default help; surfaced with --advanced.

func newBakeImageCommand() *cobra.Command {
	var providerName, k3sVersion string
	cmd := &cobra.Command{
		Use:   "bake-image",
		Short: "Build a new node image (AMI / snapshot) and tag it as bonsai-node:latest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := selectProvider(cmd.Context(), providerName)
			if err != nil {
				return err
			}
			id, err := p.BakeImage(cmd.Context(), k3sVersion)
			if err != nil {
				return err
			}
			fmt.Printf("baked %s — promoted to bonsai-node:latest\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "aws", "")
	cmd.Flags().StringVar(&k3sVersion, "k3s", "", "k3s version to pre-install (default: pinned)")
	return cmd
}

func newRotateWorkersCommand() *cobra.Command {
	var name, env, ami, providerName string
	cmd := &cobra.Command{
		Use:   "rotate-workers",
		Short: "Replace every worker node, rolling, with the latest (or specified) image",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := selectProvider(cmd.Context(), providerName)
			if err != nil {
				return err
			}
			if err := p.RotateWorkers(cmd.Context(), name, env, ami); err != nil {
				return err
			}
			fmt.Printf("instance refresh started for %s/%s (ami=%s)\n", name, env, ami)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "aws", "")
	cmd.Flags().StringVar(&name, "name", "", "")
	cmd.Flags().StringVar(&env, "env", "dev", "")
	cmd.Flags().StringVar(&ami, "image", "latest", "image reference: 'latest' or provider-native ID")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newRotateControlCommand() *cobra.Command {
	var name, env, providerName string
	cmd := &cobra.Command{
		Use:   "rotate-control",
		Short: "Snapshot state, recreate control plane from latest image, restore on boot",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := selectProvider(cmd.Context(), providerName)
			if err != nil {
				return err
			}
			if err := p.RotateControl(cmd.Context(), name, env); err != nil {
				return err
			}
			fmt.Printf("control plane rotated for %s/%s\n", name, env)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "aws", "")
	cmd.Flags().StringVar(&name, "name", "", "")
	cmd.Flags().StringVar(&env, "env", "dev", "")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newUpgradeCommand() *cobra.Command {
	var k3sVersion, component, name, env, providerName string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade k3s or an in-cluster component",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if k3sVersion == "" && component == "" {
				return fmt.Errorf("specify --k3s <version> or --component <name>")
			}
			if k3sVersion != "" && component != "" {
				return fmt.Errorf("--k3s and --component are mutually exclusive")
			}
			p, err := selectProvider(cmd.Context(), providerName)
			if err != nil {
				return err
			}
			if k3sVersion != "" {
				if err := p.UpgradeK3s(cmd.Context(), name, env, k3sVersion); err != nil {
					return err
				}
				fmt.Printf("k3s upgrade Plan applied for %s/%s → %s\n", name, env, k3sVersion)
				fmt.Println("watch progress with: kubectl get plans -n system-upgrade")
				return nil
			}
			if err := p.UpgradeComponent(cmd.Context(), name, env, component); err != nil {
				return err
			}
			fmt.Printf("%s upgraded to pinned version for %s/%s\n", component, name, env)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "aws", "")
	cmd.Flags().StringVar(&name, "name", "", "")
	cmd.Flags().StringVar(&env, "env", "dev", "")
	cmd.Flags().StringVar(&k3sVersion, "k3s", "", "k3s target version, e.g. v1.31.0+k3s1")
	cmd.Flags().StringVar(&component, "component", "", "cnpg | valkey | kured | system-upgrade-controller")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newDestroyCommand() *cobra.Command {
	var name, env, providerName string
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Tear down a cluster (irreversible)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := selectProvider(cmd.Context(), providerName)
			if err != nil {
				return err
			}
			return p.Destroy(cmd.Context(), name, env)
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "aws", "")
	cmd.Flags().StringVar(&name, "name", "", "")
	cmd.Flags().StringVar(&env, "env", "dev", "")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}
