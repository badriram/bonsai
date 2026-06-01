package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_minimalHetznerHA(t *testing.T) {
	tskey := writeTempFile(t, "key", "tskey-client-abc123-xyz\n")
	cfgPath := writeTempFile(t, "bonsai.yaml", `
name: smoke
env: dev
provider: hetzner
workers: 2
ha_control: true
control_server_type: cpx22
worker_server_type: cpx22
locations: [nbg1, fsn1, hel1]
k3s_version: v1.31.0+k3s1
postgres:
  instances: 2
tailnet:
  enabled: true
  login_server: https://controlplane.tailscale.com
  tag: tag:bonsai
  auth_key_file: `+tskey+`
`)
	cc, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cc.Provider != "hetzner" || cc.Name != "smoke" || !cc.HAControl || cc.Workers != 2 {
		t.Fatalf("bad parse: %+v", cc)
	}
	if !cc.TailnetMode() {
		t.Fatal("expected tailnet mode")
	}
	if cc.ControlServerType != "cpx22" || cc.WorkerServerType != "cpx22" {
		t.Fatalf("server types not threaded: %+v", cc)
	}
	if len(cc.Locations) != 3 {
		t.Fatalf("locations: %+v", cc.Locations)
	}
	if cc.PostgresInstances != 2 {
		t.Fatalf("postgres.instances: %d", cc.PostgresInstances)
	}
}

func TestLoad_rejectsMissingAdminCIDRWhenNonTailnet(t *testing.T) {
	t.Setenv("BONSAI_ADMIN_CIDR", "") // no env fallback
	cfgPath := writeTempFile(t, "bonsai.yaml", `
name: smoke
env: dev
provider: hetzner
workers: 2
ha_control: true
`)
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "admin_cidr") {
		t.Fatalf("expected admin_cidr error, got %v", err)
	}
}

func TestLoad_rejectsMalformedTailnetCred(t *testing.T) {
	bad := writeTempFile(t, "bad-key", "Client ID: foo\nNo tskey token anywhere\n")
	cfgPath := writeTempFile(t, "bonsai.yaml", `
name: smoke
env: dev
provider: hetzner
ha_control: true
tailnet:
  enabled: true
  login_server: https://controlplane.tailscale.com
  auth_key_file: `+bad+`
`)
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "tskey-") {
		t.Fatalf("expected tskey- token error, got %v", err)
	}
}

func TestLoad_acceptsMultilinePasteWithTskey(t *testing.T) {
	// The exact format that broke bug #11 — Tailscale admin UI paste.
	paste := writeTempFile(t, "ts-paste", "Client ID: k1X3ocAo\nClient secret: tskey-client-abc-def\n")
	cfgPath := writeTempFile(t, "bonsai.yaml", `
name: smoke
env: dev
provider: hetzner
ha_control: true
tailnet:
  enabled: true
  login_server: https://controlplane.tailscale.com
  auth_key_file: `+paste+`
`)
	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoad_rejectsTailnetAuthOnWrongProvider(t *testing.T) {
	cfgPath := writeTempFile(t, "bonsai.yaml", `
name: smoke
env: dev
provider: aws
ha_control: true
tailnet:
  enabled: true
  login_server: https://controlplane.tailscale.com
  auth_key_file: /tmp/whatever
`)
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "auth_key_ssm") {
		t.Fatalf("expected auth_key_ssm error for aws, got %v", err)
	}
}

func TestLoad_rejectsUnknownFields(t *testing.T) {
	cfgPath := writeTempFile(t, "bonsai.yaml", `
name: smoke
env: dev
provider: hetzner
admin_cidr: 1.2.3.4/32
mystery_field: oops
`)
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "mystery_field") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}
