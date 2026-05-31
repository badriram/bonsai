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

func renderServerUserData(v serverVars) (string, error)   { return render("userdata/server.sh.tmpl", v) }
func renderWorkerUserData(v workerVars) (string, error)   { return render("userdata/worker.sh.tmpl", v) }
func renderBuilderUserData(v builderVars) (string, error) { return render("userdata/builder.sh.tmpl", v) }
func renderRestoreUserData(v restoreVars) (string, error) { return render("userdata/restore.sh.tmpl", v) }

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
