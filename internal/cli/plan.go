package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	bcfg "github.com/badriram/bonsai/internal/config"
	"github.com/badriram/bonsai/internal/state"
)

// `bonsai plan` is the readonly counterpart to `bonsai grow`. It diffs three
// sources of truth — declared (bonsai.yaml), last-known (state.json),
// observed (cloud API) — and reports what a grow would do, without
// mutating anything.
//
// Exit codes (CI-friendly):
//   0 — no changes
//   2 — changes pending or drift detected
//   1 — error (config invalid, cloud creds missing, etc.)
func newPlanCommand() *cobra.Command {
	var cfg bcfg.ClusterConfig
	var configPath string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Diff desired (bonsai.yaml) vs last-known (state.json) vs observed (cloud) — no mutation",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveGrowConfig(cmd, cfg, configPath)
			if err != nil {
				return err
			}

			dataDir := resolveDataDir()
			st, err := state.Read(state.Path(dataDir, resolved.Name, resolved.Env))
			if err != nil {
				return fmt.Errorf("read state: %w", err)
			}

			changes, err := buildPlan(cmd.Context(), resolved, st)
			if err != nil {
				return err
			}

			printPlan(resolved, st, changes)
			if len(changes) > 0 {
				os.Exit(2)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to bonsai.yaml (auto-discovers ./bonsai.yaml if unset)")
	cmd.Flags().StringVar(&cfg.Provider, "provider", "aws", "aws | hetzner")
	cmd.Flags().StringVar(&cfg.Name, "name", "", "app name")
	cmd.Flags().StringVar(&cfg.Env, "env", "dev", "environment")
	cmd.Flags().IntVar(&cfg.Workers, "workers", 1, "")
	cmd.Flags().StringVar(&cfg.Region, "region", "us-east-1", "")
	cmd.Flags().BoolVar(&cfg.HAControl, "ha-control", false, "")
	cmd.Flags().StringVar(&cfg.AdminCIDR, "admin-cidr", "", "")
	cmd.Flags().StringVar(&cfg.TailnetURL, "tailnet-url", "", "")
	cmd.Flags().StringVar(&cfg.TailnetKeyFile, "tailnet-key-file", "", "")
	cmd.Flags().StringVar(&cfg.TailnetKeySSMPath, "tailnet-key-ssm", "", "")
	cmd.Flags().StringVar(&cfg.TailnetTag, "tailnet-tag", "tag:bonsai", "")
	return cmd
}

// change is one line of plan output. severity drives ordering and color
// (color deferred to a follow-up).
type change struct {
	kind     string // CREATE | UPDATE | REPLACE | DRIFT | WARN
	resource string // e.g. "worker", "control_server_type", "network"
	msg      string
}

func buildPlan(ctx context.Context, cfg bcfg.ClusterConfig, st *state.State) ([]change, error) {
	switch cfg.Provider {
	case "hetzner":
		return planHetzner(ctx, cfg, st)
	case "aws":
		return nil, fmt.Errorf("bonsai plan: aws provider not yet supported (state.json + plan landing incrementally)")
	case "libvirt":
		return planLibvirt(cfg, st), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}

// planLibvirt does the desired-vs-state diff only. There's no remote
// catalog to consult (the VM types are local CPU/RAM choices), so we don't
// emit WARN lines for SKU deprecation here.
func planLibvirt(cfg bcfg.ClusterConfig, st *state.State) []change {
	if st == nil {
		ctrl := 1
		if cfg.HAControl {
			ctrl = 3
		}
		workers := cfg.Workers
		if workers < 1 {
			workers = 1
		}
		return []change{
			{kind: "CREATE", resource: "control_plane", msg: fmt.Sprintf("%d libvirt VM(s)", ctrl)},
			{kind: "CREATE", resource: "workers", msg: fmt.Sprintf("%d libvirt VM(s)", workers)},
		}
	}
	return diffDeclaredVsDesired(st.Declared, cfg)
}

// planHetzner runs the diff checks against Hetzner Cloud. Read-only API
// calls only: list servers/networks/firewalls by label + verify the
// declared server_type still exists and isn't deprecated.
func planHetzner(ctx context.Context, cfg bcfg.ClusterConfig, st *state.State) ([]change, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("HCLOUD_TOKEN not set")
	}
	client := hcloud.NewClient(hcloud.WithToken(token))

	var changes []change

	// Server-type validity check. cx22 deprecated mid-2026 was bug #1 of the
	// Hetzner smoke — catching it at plan time was the load-bearing motivation
	// for this command.
	for _, st := range []string{cfg.ControlServerType, cfg.WorkerServerType} {
		if st == "" {
			continue
		}
		t, _, err := client.ServerType.GetByName(ctx, st)
		if err != nil {
			return nil, fmt.Errorf("hetzner ServerType.GetByName(%s): %w", st, err)
		}
		if t == nil {
			changes = append(changes, change{kind: "WARN", resource: "server_type", msg: fmt.Sprintf("%q does not exist in Hetzner Cloud", st)})
			continue
		}
		if t.Deprecation != nil {
			changes = append(changes, change{kind: "WARN", resource: "server_type", msg: fmt.Sprintf("%q is deprecated by Hetzner (announced %s, unavailable after %s)", st, t.Deprecation.Announced.Format("2006-01-02"), t.Deprecation.UnavailableAfter.Format("2006-01-02"))})
		}
	}

	// First grow path — no state yet, everything is CREATE.
	if st == nil {
		changes = append(changes,
			change{kind: "CREATE", resource: "network", msg: "Hetzner Network 10.0.0.0/16"},
			change{kind: "CREATE", resource: "firewall", msg: "Hetzner Cloud Firewall"},
		)
		if !cfg.TailnetMode() {
			changes = append(changes, change{kind: "CREATE", resource: "load_balancer", msg: "Hetzner LB (lb11)"})
		}
		ctrl := 1
		if cfg.HAControl {
			ctrl = 3
		}
		changes = append(changes, change{kind: "CREATE", resource: "control_plane", msg: fmt.Sprintf("%d node(s), type=%s", ctrl, nonEmpty(cfg.ControlServerType, "<provider default>"))})
		workers := cfg.Workers
		if workers < 1 {
			workers = 1
		}
		changes = append(changes, change{kind: "CREATE", resource: "workers", msg: fmt.Sprintf("%d node(s), type=%s", workers, nonEmpty(cfg.WorkerServerType, "<provider default>"))})
		return changes, nil
	}

	// We have state. Compare last-known declared vs current desired, and
	// state-known resource IDs vs observed cloud.
	changes = append(changes, diffDeclaredVsDesired(st.Declared, cfg)...)

	// Drift check: do the state-known servers still exist in Hetzner?
	if st.Hetzner != nil {
		live, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
			ListOpts: hcloud.ListOpts{LabelSelector: "bonsai.cluster=" + cfg.Name + ",bonsai.env=" + cfg.Env},
		})
		if err != nil {
			return nil, fmt.Errorf("hetzner Server.All: %w", err)
		}
		liveIDs := map[int64]bool{}
		for _, s := range live {
			liveIDs[s.ID] = true
		}
		for _, srv := range st.Hetzner.Servers {
			if !liveIDs[srv.ID] {
				changes = append(changes, change{kind: "DRIFT", resource: "server", msg: fmt.Sprintf("%s (id=%d) tracked in state but not in cloud — re-create on next grow", srv.Name, srv.ID)})
			}
		}
		// Servers in cloud not in state (manual creation by operator).
		stateIDs := map[int64]bool{}
		for _, s := range st.Hetzner.Servers {
			stateIDs[s.ID] = true
		}
		for _, s := range live {
			if !stateIDs[s.ID] {
				changes = append(changes, change{kind: "DRIFT", resource: "server", msg: fmt.Sprintf("%s (id=%d) exists in cloud but not in state — manually created? grow will not adopt it", s.Name, s.ID)})
			}
		}
	}

	return changes, nil
}

// diffDeclaredVsDesired compares the last-known declared config (from state)
// to the operator's current intent (from bonsai.yaml + flags). Pure
// function — no cloud calls — so it's the testable core of plan logic.
func diffDeclaredVsDesired(was, want bcfg.ClusterConfig) []change {
	var out []change
	if was.Workers != want.Workers {
		out = append(out, change{kind: "UPDATE", resource: "workers", msg: fmt.Sprintf("count %d → %d", was.Workers, want.Workers)})
	}
	if want.ControlServerType != "" && was.ControlServerType != want.ControlServerType {
		out = append(out, change{kind: "REPLACE", resource: "control_server_type", msg: fmt.Sprintf("%q → %q (destructive — rotate-control re-creates each node)", was.ControlServerType, want.ControlServerType)})
	}
	if want.WorkerServerType != "" && was.WorkerServerType != want.WorkerServerType {
		out = append(out, change{kind: "REPLACE", resource: "worker_server_type", msg: fmt.Sprintf("%q → %q (destructive — rotate-workers re-creates each node)", was.WorkerServerType, want.WorkerServerType)})
	}
	if want.K3sVersion != "" && was.K3sVersion != want.K3sVersion {
		out = append(out, change{kind: "UPDATE", resource: "k3s_version", msg: fmt.Sprintf("%s → %s", nonEmpty(was.K3sVersion, "<provider default>"), want.K3sVersion)})
	}
	if want.AdminCIDR != was.AdminCIDR {
		out = append(out, change{kind: "UPDATE", resource: "admin_cidr", msg: fmt.Sprintf("%q → %q", was.AdminCIDR, want.AdminCIDR)})
	}
	if was.TailnetMode() != want.TailnetMode() {
		out = append(out, change{kind: "REPLACE", resource: "tailnet_mode", msg: fmt.Sprintf("toggle %v → %v (architecture change — destroy + grow required)", was.TailnetMode(), want.TailnetMode())})
	}
	if c := diffPostgresVolumeSize(was, want); c != nil {
		out = append(out, *c)
	}
	return out
}

// diffPostgresVolumeSize reads as: shrinks are never applied (CSI rejects;
// data-loss risk), grows are online on AWS/Hetzner via CSI volume expansion,
// and libvirt's qcow2 ceiling is fixed at first grow so any change there
// requires destroy+grow.
func diffPostgresVolumeSize(was, want bcfg.ClusterConfig) *change {
	if want.PostgresVolumeSize == "" || was.PostgresVolumeSize == "" {
		return nil
	}
	if was.PostgresVolumeSize == want.PostgresVolumeSize {
		return nil
	}
	wantQ, err1 := resource.ParseQuantity(want.PostgresVolumeSize)
	wasQ, err2 := resource.ParseQuantity(was.PostgresVolumeSize)
	if err1 != nil || err2 != nil {
		return nil
	}
	cmp := wantQ.Cmp(wasQ)
	if cmp < 0 {
		return &change{kind: "WARN", resource: "postgres.volume_size", msg: fmt.Sprintf("%s → %s ignored — Bonsai never shrinks Postgres storage; existing %s kept", was.PostgresVolumeSize, want.PostgresVolumeSize, was.PostgresVolumeSize)}
	}
	if want.Provider == "libvirt" {
		return &change{kind: "WARN", resource: "postgres.volume_size", msg: fmt.Sprintf("%s → %s requires destroy + grow on libvirt — qcow2 ceiling is fixed at provision time", was.PostgresVolumeSize, want.PostgresVolumeSize)}
	}
	return &change{kind: "UPDATE", resource: "postgres.volume_size", msg: fmt.Sprintf("%s → %s (online PVC expansion — primary stays up)", was.PostgresVolumeSize, want.PostgresVolumeSize)}
}

func printPlan(cfg bcfg.ClusterConfig, st *state.State, changes []change) {
	fmt.Printf("cluster:   %s/%s\n", cfg.Name, cfg.Env)
	fmt.Printf("provider:  %s\n", cfg.Provider)
	if st == nil {
		fmt.Println("state:     <no state.json — first grow>")
	} else {
		fmt.Printf("state:     last grown by %s at %s\n", st.BonsaiVersion, st.ProvisionedAt.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Println()
	if len(changes) == 0 {
		fmt.Println("no changes — bonsai grow would be a no-op")
		return
	}
	// Group by kind for readability. Order: WARN, REPLACE, DRIFT, CREATE, UPDATE.
	order := []string{"WARN", "REPLACE", "DRIFT", "CREATE", "UPDATE"}
	for _, k := range order {
		for _, c := range changes {
			if c.kind == k {
				fmt.Printf("  %-8s %-20s %s\n", c.kind, c.resource, c.msg)
			}
		}
	}
	fmt.Printf("\n%d change(s) pending. Run `bonsai grow` to apply.\n", len(changes))
}

func resolveDataDir() string {
	dir := os.Getenv("BONSAI_DATA_DIR")
	if dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bonsai")
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
