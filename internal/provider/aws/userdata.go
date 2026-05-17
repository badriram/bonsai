package aws

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"text/template"
)

//go:embed userdata/*.tmpl
var userdataFS embed.FS

type serverVars struct {
	Name, Env, Region, ControlIP, BackupBucket string
}

type workerVars struct {
	Name, Env, Region, ControlPlaneURL string
}

type builderVars struct {
	K3sVersion string
}

func renderServerUserData(v serverVars) (string, error) {
	return renderUserData("userdata/server.sh.tmpl", v)
}

func renderWorkerUserData(v workerVars) (string, error) {
	return renderUserData("userdata/worker.sh.tmpl", v)
}

func renderBuilderUserData(v builderVars) (string, error) {
	return renderUserData("userdata/builder.sh.tmpl", v)
}

func renderUserData(path string, v any) (string, error) {
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
	// EC2 user-data must be base64-encoded for RunInstances / launch templates.
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
