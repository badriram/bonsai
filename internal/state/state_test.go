package state

import (
	"os"
	"path/filepath"
	"testing"

	bcfg "github.com/badriram/bonsai/internal/config"
)

func TestReadAbsentReturnsNilNil(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil state, got %+v", got)
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "smoke", "dev")
	s := &State{
		BonsaiVersion: "v0.2.1",
		Declared:      bcfg.ClusterConfig{Name: "smoke", Env: "dev", Provider: "hetzner", Workers: 2},
		Hetzner: &HetznerState{
			NetworkID: 12345,
			K3sVersion: "v1.31.0+k3s1",
			Servers: []HetznerServer{
				{ID: 1, Name: "ctrl-1", Role: "control-plane", Location: "nbg1", ServerType: "cpx22", PrivateIP: "10.0.3.2"},
			},
		},
	}
	if err := Write(path, s); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// tmp file must not linger after success
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file leaked: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got == nil || got.SchemaVersion != SchemaVersion {
		t.Fatalf("bad read: %+v", got)
	}
	if got.Declared.Name != "smoke" || got.Hetzner == nil || got.Hetzner.NetworkID != 12345 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.ProvisionedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", got)
	}
}

func TestWritePreservesProvisionedAt(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "smoke", "dev")
	first := &State{
		Declared: bcfg.ClusterConfig{Name: "smoke", Env: "dev", Provider: "hetzner"},
	}
	if err := Write(path, first); err != nil {
		t.Fatal(err)
	}
	provisioned := first.ProvisionedAt

	loaded, _ := Read(path)
	loaded.Declared.Workers = 5
	if err := Write(path, loaded); err != nil {
		t.Fatal(err)
	}
	if !loaded.ProvisionedAt.Equal(provisioned) {
		t.Fatalf("ProvisionedAt should not change on update: was %v, now %v", provisioned, loaded.ProvisionedAt)
	}
	if !loaded.UpdatedAt.After(provisioned) && !loaded.UpdatedAt.Equal(provisioned) {
		t.Fatalf("UpdatedAt should be >= ProvisionedAt")
	}
}

func TestReadStaleSchemaReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "smoke", "dev")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(`{"schema_version":"v0","bonsai_version":"v0.1.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for stale schema, got %+v", got)
	}
}
