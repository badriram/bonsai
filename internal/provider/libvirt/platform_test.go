package libvirt

import (
	"runtime"
	"strings"
	"testing"
)

func TestDefaultDomainType_RespectsEnv(t *testing.T) {
	t.Setenv("BONSAI_LIBVIRT_DOMAIN_TYPE", "hvf")
	if got := defaultDomainType(); got != "hvf" {
		t.Fatalf("env override ignored: got %q", got)
	}
}

func TestDefaultDomainType_HostFallback(t *testing.T) {
	t.Setenv("BONSAI_LIBVIRT_DOMAIN_TYPE", "")
	want := "kvm"
	if runtime.GOOS == "darwin" {
		want = "hvf"
	}
	if got := defaultDomainType(); got != want {
		t.Fatalf("host fallback: got %q, want %q (GOOS=%s)", got, want, runtime.GOOS)
	}
}

func TestDefaultBaseImageURL_RespectsEnv(t *testing.T) {
	t.Setenv("BONSAI_LIBVIRT_IMAGE_URL", "https://example.test/custom.qcow2")
	if got := defaultBaseImageURL(); got != "https://example.test/custom.qcow2" {
		t.Fatalf("env override ignored: got %q", got)
	}
}

func TestDefaultBaseImageURL_PicksByArch(t *testing.T) {
	t.Setenv("BONSAI_LIBVIRT_IMAGE_URL", "")
	got := defaultBaseImageURL()
	wantArch := "x86_64"
	if runtime.GOARCH == "arm64" {
		wantArch = "aarch64"
	}
	if !strings.Contains(got, wantArch) {
		t.Fatalf("default image URL %q does not include arch %q (GOARCH=%s)", got, wantArch, runtime.GOARCH)
	}
	if !strings.Contains(got, "cloudinit") {
		t.Fatalf("default image URL %q is not a cloud-init variant", got)
	}
}

func TestLibvirtArch(t *testing.T) {
	a, m := libvirtArch()
	switch runtime.GOARCH {
	case "arm64":
		if a != "aarch64" || m != "virt" {
			t.Fatalf("arm64 host: got arch=%s machine=%s", a, m)
		}
	default:
		if a != "x86_64" || m != "q35" {
			t.Fatalf("non-arm64 host: got arch=%s machine=%s", a, m)
		}
	}
}
