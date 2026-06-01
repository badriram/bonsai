package libvirt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// buildNoCloudISO creates a NoCloud-format config drive ISO with the
// supplied user-data + meta-data. The Alpine cloud image's cloud-init
// picks it up automatically (datasource = NoCloud, source = cidata
// volume label).
//
// user-data is delivered as a #!/bin/sh shebang script — cloud-init's
// scripts-user module runs it verbatim. We don't use #cloud-config YAML
// because that's exactly the bug-#11 shape we just spent a release fixing.
func buildNoCloudISO(ctx context.Context, isoPath, hostname, userDataScript, sshPub string) error {
	stage, err := os.MkdirTemp("", "bonsai-nocloud-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	metaData := fmt.Sprintf("instance-id: bonsai-%s\nlocal-hostname: %s\npublic-keys:\n  - %s\n",
		hostname, hostname, sshPub)
	if err := os.WriteFile(filepath.Join(stage, "meta-data"), []byte(metaData), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "user-data"), []byte(userDataScript), 0o600); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, binISO(),
		"-output", isoPath,
		"-volid", "cidata",
		"-joliet", "-rock",
		filepath.Join(stage, "meta-data"),
		filepath.Join(stage, "user-data"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w (output: %s)", binISO(), err, string(out))
	}
	return nil
}
