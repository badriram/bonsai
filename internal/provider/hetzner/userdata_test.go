package hetzner

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestServerUserDataCloudConfigShape locks in the contract that
// server.sh.tmpl renders as parseable cloud-config YAML with the
// Bonsai-injected host key in the right place. cloud-init treats this as
// authoritative — a malformed ssh_keys block silently drops the host key
// and brings us right back to the InsecureIgnoreHostKey foot-gun.
func TestServerUserDataCloudConfigShape(t *testing.T) {
	const fakePEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAAA\nBBBBBB\n-----END OPENSSH PRIVATE KEY-----\n"
	out, err := renderServerUserData(serverVars{
		ControlIP:              "1.2.3.4",
		K3sVersion:             "v1.31.0+k3s1",
		HostKeyPublic:          "ssh-ed25519 AAAATESTKEY bonsai",
		HostKeyPrivateIndented: indentForCloudConfig(fakePEM, 4),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.HasPrefix(out, "#cloud-config") {
		t.Fatalf("expected #cloud-config preamble, got: %s", firstLine(out))
	}

	// Strip the comment line; gopkg.in/yaml.v3 handles the rest.
	var doc struct {
		SSHKeys struct {
			Ed25519Private string `yaml:"ed25519_private"`
			Ed25519Public  string `yaml:"ed25519_public"`
		} `yaml:"ssh_keys"`
		Runcmd []string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("cloud-config did not parse as YAML: %v\n---\n%s", err, out)
	}
	if !strings.Contains(doc.SSHKeys.Ed25519Private, "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatalf("private key missing or mangled in ssh_keys.ed25519_private:\n%q", doc.SSHKeys.Ed25519Private)
	}
	if !strings.Contains(doc.SSHKeys.Ed25519Private, "END OPENSSH PRIVATE KEY") {
		t.Fatalf("private key truncated:\n%q", doc.SSHKeys.Ed25519Private)
	}
	if !strings.Contains(doc.SSHKeys.Ed25519Public, "AAAATESTKEY") {
		t.Fatalf("public key not propagated: %q", doc.SSHKeys.Ed25519Public)
	}
	if len(doc.Runcmd) == 0 || !strings.Contains(doc.Runcmd[0], "INSTALL_K3S_VERSION=\"v1.31.0+k3s1\"") {
		t.Fatalf("runcmd missing k3s install with templated version: %v", doc.Runcmd)
	}
	if !strings.Contains(doc.Runcmd[0], "--tls-san=\"1.2.3.4\"") {
		t.Fatalf("runcmd missing templated control IP in --tls-san: %v", doc.Runcmd)
	}
}

func TestRestoreUserDataCloudConfigShape(t *testing.T) {
	const fakePEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"
	out, err := renderRestoreUserData(restoreVars{
		HostKeyPublic:          "ssh-ed25519 AAAATESTKEY bonsai",
		HostKeyPrivateIndented: indentForCloudConfig(fakePEM, 4),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc struct {
		SSHKeys struct {
			Ed25519Private string `yaml:"ed25519_private"`
			Ed25519Public  string `yaml:"ed25519_public"`
		} `yaml:"ssh_keys"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("restore cloud-config did not parse: %v\n---\n%s", err, out)
	}
	if !strings.Contains(doc.SSHKeys.Ed25519Private, "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatalf("restore: private key missing")
	}
	if !strings.Contains(doc.SSHKeys.Ed25519Public, "AAAATESTKEY") {
		t.Fatalf("restore: public key missing: %q", doc.SSHKeys.Ed25519Public)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
