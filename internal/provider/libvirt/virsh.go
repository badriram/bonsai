package libvirt

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// virsh runs `virsh -c <uri> <args...>` and returns its stdout. Stderr is
// folded into the error on failure so the caller sees libvirt's own
// message instead of a generic exit-1.
func virsh(ctx context.Context, uri string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "virsh", append([]string{"-c", uri}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("virsh %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// qemuImg shells out to qemu-img. Wrapper exists so tests can swap exec.
func qemuImg(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "qemu-img", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("qemu-img %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// binISO returns the name of the ISO-builder binary expected on PATH.
// genisoimage is Debian/Ubuntu/Alpine; mkisofs is older / macOS via brew.
// Linux operators use genisoimage; macOS dev boxes use mkisofs; we accept
// either and the New() PATH check picks whichever exists.
func binISO() string {
	if runtime.GOOS == "darwin" {
		return "mkisofs"
	}
	return "genisoimage"
}
