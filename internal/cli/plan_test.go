package cli

import (
	"strings"
	"testing"

	bcfg "github.com/badriram/bonsai/internal/config"
)

func TestDiffDeclared_noChange(t *testing.T) {
	base := bcfg.ClusterConfig{Name: "smoke", Env: "dev", Provider: "hetzner", Workers: 2, ControlServerType: "cpx22", WorkerServerType: "cpx22"}
	if got := diffDeclaredVsDesired(base, base); len(got) != 0 {
		t.Fatalf("expected no changes, got %+v", got)
	}
}

func TestDiffDeclared_workerCount(t *testing.T) {
	was := bcfg.ClusterConfig{Workers: 2}
	want := bcfg.ClusterConfig{Workers: 5}
	changes := diffDeclaredVsDesired(was, want)
	if len(changes) != 1 || changes[0].kind != "UPDATE" || changes[0].resource != "workers" {
		t.Fatalf("expected single UPDATE workers, got %+v", changes)
	}
	if !strings.Contains(changes[0].msg, "2 → 5") {
		t.Fatalf("message lost the diff: %q", changes[0].msg)
	}
}

func TestDiffDeclared_serverTypeReplace(t *testing.T) {
	was := bcfg.ClusterConfig{ControlServerType: "cpx22"}
	want := bcfg.ClusterConfig{ControlServerType: "cax21"}
	changes := diffDeclaredVsDesired(was, want)
	if len(changes) != 1 || changes[0].kind != "REPLACE" {
		t.Fatalf("expected REPLACE, got %+v", changes)
	}
	if !strings.Contains(changes[0].msg, "destructive") {
		t.Fatalf("expected destructive warning, got %q", changes[0].msg)
	}
}

func TestDiffDeclared_tailnetToggleIsReplace(t *testing.T) {
	was := bcfg.ClusterConfig{} // tailnet off
	want := bcfg.ClusterConfig{
		TailnetURL:     "https://controlplane.tailscale.com",
		TailnetKeyFile: "/some/path",
	}
	changes := diffDeclaredVsDesired(was, want)
	if len(changes) != 1 || changes[0].kind != "REPLACE" || changes[0].resource != "tailnet_mode" {
		t.Fatalf("expected REPLACE tailnet_mode, got %+v", changes)
	}
	if !strings.Contains(changes[0].msg, "architecture change") {
		t.Fatalf("expected architecture-change warning, got %q", changes[0].msg)
	}
}

func TestDiffDeclared_emptyDefaultIgnored(t *testing.T) {
	// If desired leaves a field empty (= "use provider default"), it should
	// not show as a change against a state that recorded the resolved type.
	was := bcfg.ClusterConfig{ControlServerType: "cpx22"}
	want := bcfg.ClusterConfig{} // empty — operator wants the default
	changes := diffDeclaredVsDesired(was, want)
	if len(changes) != 0 {
		t.Fatalf("empty desired should not trigger change, got %+v", changes)
	}
}

func TestDiffDeclared_postgresVolumeGrowOnHetzner(t *testing.T) {
	was := bcfg.ClusterConfig{Provider: "hetzner", PostgresVolumeSize: "10Gi"}
	want := bcfg.ClusterConfig{Provider: "hetzner", PostgresVolumeSize: "50Gi"}
	changes := diffDeclaredVsDesired(was, want)
	if len(changes) != 1 || changes[0].kind != "UPDATE" || changes[0].resource != "postgres.volume_size" {
		t.Fatalf("expected UPDATE postgres.volume_size, got %+v", changes)
	}
	if !strings.Contains(changes[0].msg, "online") {
		t.Fatalf("expected online expansion note, got %q", changes[0].msg)
	}
}

func TestDiffDeclared_postgresVolumeGrowOnLibvirtWarns(t *testing.T) {
	was := bcfg.ClusterConfig{Provider: "libvirt", PostgresVolumeSize: "10Gi"}
	want := bcfg.ClusterConfig{Provider: "libvirt", PostgresVolumeSize: "50Gi"}
	changes := diffDeclaredVsDesired(was, want)
	if len(changes) != 1 || changes[0].kind != "WARN" {
		t.Fatalf("expected WARN, got %+v", changes)
	}
	if !strings.Contains(changes[0].msg, "destroy + grow") {
		t.Fatalf("expected destroy + grow guidance, got %q", changes[0].msg)
	}
}

func TestDiffDeclared_postgresVolumeShrinkWarnsRegardlessOfProvider(t *testing.T) {
	for _, prov := range []string{"hetzner", "aws", "libvirt"} {
		was := bcfg.ClusterConfig{Provider: prov, PostgresVolumeSize: "50Gi"}
		want := bcfg.ClusterConfig{Provider: prov, PostgresVolumeSize: "10Gi"}
		changes := diffDeclaredVsDesired(was, want)
		if len(changes) != 1 || changes[0].kind != "WARN" {
			t.Fatalf("provider %s: expected WARN on shrink, got %+v", prov, changes)
		}
		if !strings.Contains(changes[0].msg, "ignored") {
			t.Fatalf("provider %s: expected explicit 'ignored', got %q", prov, changes[0].msg)
		}
	}
}

func TestDiffDeclared_postgresVolumeUnchangedIsQuiet(t *testing.T) {
	was := bcfg.ClusterConfig{Provider: "aws", PostgresVolumeSize: "10Gi"}
	want := bcfg.ClusterConfig{Provider: "aws", PostgresVolumeSize: "10Gi"}
	if changes := diffDeclaredVsDesired(was, want); len(changes) != 0 {
		t.Fatalf("expected no changes for unchanged volume size, got %+v", changes)
	}
}
