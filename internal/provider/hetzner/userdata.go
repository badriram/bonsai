package hetzner

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed userdata/*.tmpl
var userdataFS embed.FS

type serverVars struct {
	ControlIP              string
	K3sVersion             string
	HostKeyPublic          string // authorized-keys form, single line
	HostKeyPrivateIndented string // PEM, every line prefixed with 4 spaces for cloud-config block scalar
}

type workerVars struct {
	ControlIP  string
	K3sVersion string
	Token      string
}

type builderVars struct {
	K3sVersion string
}

type restoreVars struct {
	HostKeyPublic          string
	HostKeyPrivateIndented string
}

// serverHAVars drives server-ha.sh.tmpl (LB-mode HA control plane).
type serverHAVars struct {
	Name, Env              string
	K3sVersion             string
	Token                  string // pre-seeded by Bonsai; same on leader + joiners
	IsLeader               bool   // leader does --cluster-init; joiners use --server
	NodePrivateIP          string // this node's IP in the cluster Hetzner Network
	LeaderPrivateIP        string // joiner-only — leader's private IP for --server
	ClusterEndpoint        string // LB public IP/DNS, baked into --tls-san
	HostKeyPublic          string
	HostKeyPrivateIndented string
}

// serverTailnetHAVars drives server-tailnet-ha.sh.tmpl (tailnet-mode HA).
type serverTailnetHAVars struct {
	Name, Env              string
	K3sVersion             string
	Token                  string
	IsLeader               bool
	NodeIndex              int    // 1, 2, 3 — for distinct tailnet hostnames
	LeaderTailnetIP        string // joiner-only
	TailnetURL             string
	TailnetAuthCred        string // raw OAuth client secret or pre-auth key
	TailnetTag             string
	HostKeyPublic          string
	HostKeyPrivateIndented string
}

// workerTailnetVars drives worker-tailnet.sh.tmpl.
type workerTailnetVars struct {
	Name, Env        string
	K3sVersion       string
	Token            string
	NodeIndex        int
	ControlTailnetIP string // leader's tailnet IP
	TailnetURL       string
	TailnetAuthCred  string
	TailnetTag       string
}

func renderServerUserData(v serverVars) (string, error)   { return render("userdata/server.sh.tmpl", v) }
func renderWorkerUserData(v workerVars) (string, error)   { return render("userdata/worker.sh.tmpl", v) }
func renderBuilderUserData(v builderVars) (string, error) { return render("userdata/builder.sh.tmpl", v) }
func renderRestoreUserData(v restoreVars) (string, error) { return render("userdata/restore.sh.tmpl", v) }

func renderServerHAUserData(v serverHAVars) (string, error) {
	return render("userdata/server-ha.sh.tmpl", v)
}
func renderServerTailnetHAUserData(v serverTailnetHAVars) (string, error) {
	return render("userdata/server-tailnet-ha.sh.tmpl", v)
}
func renderWorkerTailnetUserData(v workerTailnetVars) (string, error) {
	return render("userdata/worker-tailnet.sh.tmpl", v)
}

// indentForCloudConfig prefixes every line with `n` spaces. Used to embed a
// multi-line PEM into a cloud-config `|` block scalar at a specific indent.
func indentForCloudConfig(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

func render(path string, v any) (string, error) {
	raw, err := userdataFS.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	tmpl, err := template.New(path).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("execute %s: %w", path, err)
	}
	return buf.String(), nil
}
