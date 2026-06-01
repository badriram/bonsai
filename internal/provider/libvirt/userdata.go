package libvirt

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed userdata/*.tmpl
var userdataFS embed.FS

type serverVars struct {
	K3sVersion string
}

type workerVars struct {
	K3sVersion string
	ControlIP  string
	Token      string
}

func renderServerScript(v serverVars) (string, error) { return render("userdata/server.sh.tmpl", v) }
func renderWorkerScript(v workerVars) (string, error) { return render("userdata/worker.sh.tmpl", v) }

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
