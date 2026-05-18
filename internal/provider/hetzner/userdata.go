package hetzner

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed userdata/*.tmpl
var userdataFS embed.FS

type serverVars struct {
	ControlIP  string
	K3sVersion string
}

type workerVars struct {
	ControlIP  string
	K3sVersion string
	Token      string
}

type builderVars struct {
	K3sVersion string
}

func renderServerUserData(v serverVars) (string, error)   { return render("userdata/server.sh.tmpl", v) }
func renderWorkerUserData(v workerVars) (string, error)   { return render("userdata/worker.sh.tmpl", v) }
func renderBuilderUserData(v builderVars) (string, error) { return render("userdata/builder.sh.tmpl", v) }
func renderRestoreUserData() (string, error)              { return render("userdata/restore.sh.tmpl", nil) }

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
