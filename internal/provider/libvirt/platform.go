package libvirt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Per-host defaults the operator can override via env. macOS and Apple Silicon
// need different choices than the Linux/x86_64 dev box; we pick automatically
// so a fresh operator just runs `bonsai grow` instead of memorising envs.
//
// Env overrides (all optional):
//
//	LIBVIRT_URI                 — libvirt connection URI
//	BONSAI_LIBVIRT_DOMAIN_TYPE  — kvm | qemu | hvf
//	BONSAI_LIBVIRT_IMAGE_URL    — full URL to a NoCloud Alpine cloud qcow2

// alpineCloudImage is the Alpine team's official cloud-init-enabled qcow2
// for each arch. UEFI variant on both — see osBlock.Firmware.
var alpineCloudImage = map[string]string{
	"amd64": "https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/cloud/nocloud_alpine-3.20.3-x86_64-uefi-cloudinit-r0.qcow2",
	"arm64": "https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/cloud/nocloud_alpine-3.20.3-aarch64-uefi-cloudinit-r0.qcow2",
}

// libvirtArch translates Go's GOARCH to the QEMU/libvirt arch name. Bonsai
// only ships images for the two we run on; other hosts must set
// BONSAI_LIBVIRT_IMAGE_URL and a matching osBlock arch via env.
func libvirtArch() (qemuArch, machine string) {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64", "virt"
	default:
		return "x86_64", "q35"
	}
}

// defaultDomainType picks the accelerator for the host. kvm on Linux (kernel
// module), qemu on macOS — libvirt added native <domain type='hvf'> in 7.10
// but brew's package still ships qemu-system-* without HVF mapped through on
// some setups, so qemu is the safe default with TCG fallback. Operators on
// new enough libvirt can override to hvf via BONSAI_LIBVIRT_DOMAIN_TYPE.
func defaultDomainType() string {
	if v := os.Getenv("BONSAI_LIBVIRT_DOMAIN_TYPE"); v != "" {
		return v
	}
	if runtime.GOOS == "darwin" {
		// hvf is the native macOS accelerator; libvirt 11+ + brew qemu 11+
		// supports <domain type='hvf'>. Falling back to qemu (TCG) is ~30x
		// slower — override via BONSAI_LIBVIRT_DOMAIN_TYPE=qemu if hvf isn't
		// available on this host.
		return "hvf"
	}
	return "kvm"
}

// defaultBaseImageURL picks the right Alpine cloud qcow2 for this host's
// architecture. macOS operators on Apple Silicon get arm64; everyone else
// gets x86_64. BONSAI_LIBVIRT_IMAGE_URL overrides for bake-image outputs or
// non-default Alpine releases.
func defaultBaseImageURL() string {
	if v := os.Getenv("BONSAI_LIBVIRT_IMAGE_URL"); v != "" {
		return v
	}
	if u, ok := alpineCloudImage[runtime.GOARCH]; ok {
		return u
	}
	return alpineCloudImage["amd64"]
}

// guestInterfaces returns the libvirt <interface> stanzas for this host. On
// Linux we attach to libvirt's default Linux-bridge NAT network. On macOS
// we return nothing — libvirt 12.4 silently drops `<source mode='shared'/>`
// on `type='user'` and falls back to SLiRP, which doesn't write host-side
// DHCP leases. Instead we inject the NIC via qemu:commandline (see
// guestQemuArgs) using vmnet-shared directly.
func guestInterfaces(vmName string) []iface {
	if runtime.GOOS == "darwin" {
		return nil
	}
	return []iface{{
		Type:   "network",
		Source: ifaceSrc{Network: defaultNetwork},
		Model:  ifaceModel{Type: "virtio"},
	}}
}

// guestQemuArgs returns the raw qemu-cmdline args needed to attach a NIC
// that libvirt itself can't (yet) express. On macOS this is the
// vmnet-shared netdev — Apple's framework gives the VM a DHCP'd
// 192.168.64.x address on a NATted subnet, with leases written to
// /var/db/dhcpd_leases keyed by MAC. Returns empty on Linux.
func guestQemuArgs(vmName string) []qemuArg {
	if runtime.GOOS != "darwin" {
		return nil
	}
	mac := deterministicMAC(vmName)
	// On aarch64 virt: virtio-net-device sits on virtio-mmio (32 transports
	// are pre-allocated by the machine type) — no PCI slot allocation
	// fight with libvirt's auto-generated pcie-root-ports. On x86_64 q35:
	// no virtio-mmio bus exists, so we fall back to virtio-net-pci and
	// attach to pci.2 (libvirt's second PCIe root port is always created).
	device := "virtio-net-device,netdev=bonsai0,mac=" + mac
	if runtime.GOARCH != "arm64" {
		device = "virtio-net-pci,netdev=bonsai0,mac=" + mac + ",bus=pci.2,addr=0x0"
	}
	return []qemuArg{
		{Value: "-netdev"}, {Value: "vmnet-shared,id=bonsai0"},
		{Value: "-device"}, {Value: device},
	}
}

// deterministicMAC derives a 02:bn:sa:1:XX:XX:XX MAC from the VM name. 02
// is the locally-administered unicast prefix (avoids OUI collisions);
// remaining 5 bytes come from SHA-256 of the name so the same VM always
// gets the same MAC across restarts. Stable MAC matters for vmnet DHCP
// lease lookups — Apple's dhcpd keys leases by MAC.
func deterministicMAC(vmName string) string {
	h := sha256.Sum256([]byte(vmName))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}

// vmnetLeasesFile is where Apple's bootpd writes shared-network leases.
const vmnetLeasesFile = "/var/db/dhcpd_leases"

// lookupVMNetIP finds the VM's IP via Apple's DHCP lease database, then
// confirms the host can actually reach it before returning. Both checks
// matter because:
//
//   - dhcpd_leases keeps an old lease for the MAC after the previous VM
//     is destroyed (sticky binding). Without a reachability probe we'd
//     return a stale IP at second 1 — before the new VM has even DHCPed.
//   - Apple's vmnet bridge (bridge100) takes seconds-to-minutes after
//     domain start to come up on the host. During that window the IP
//     is in the lease table but the host has no route to it — every
//     SSH attempt fails "no route to host".
//
// We probe TCP/22 with a 2s connect; success means the new VM is on the
// wire and sshd is at least listening.
func lookupVMNetIP(ctx context.Context, mac string, timeout time.Duration) (string, error) {
	want := strings.ToLower(strings.ReplaceAll(mac, ":", ""))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(vmnetLeasesFile)
		if err == nil {
			if ip := matchLeaseIP(string(raw), want); ip != "" {
				if reachableTCP(ip+":22", 2*time.Second) {
					return ip, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("MAC %s never reached a reachable IP via vmnet within %s", mac, timeout)
}

// reachableTCP returns true when the host has a route to the given
// address. On macOS we shell out to /usr/bin/nc because the kernel
// rejects connect() syscalls from non-Apple-signed binaries to
// vmnet-bridged destinations — Go's net.DialTimeout fails with "no
// route to host" against the same IP nc connects to fine. nc is
// Apple-signed and gets implicit grant; Bonsai is ad-hoc-signed and
// doesn't. On Linux we use Go's net.DialTimeout directly.
//
// "Refused" counts as reachable: the IP responded with RST, proving
// the bridge is up and the VM kernel reachable even if sshd isn't
// listening yet.
func reachableTCP(addr string, timeout time.Duration) bool {
	if runtime.GOOS == "darwin" {
		return reachableTCPDarwin(addr, timeout)
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err == nil {
		_ = conn.Close()
		return true
	}
	if strings.Contains(err.Error(), "connection refused") {
		return true
	}
	return false
}

func reachableTCPDarwin(addr string, timeout time.Duration) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	cmd := exec.Command("/usr/bin/nc", "-z", "-G", fmt.Sprint(seconds), host, port)
	return cmd.Run() == nil
}

// matchLeaseIP returns the ip_address for the block whose hw_address ends
// with `wantHex` (MAC lowercased, colons stripped — Apple stores it
// without separators behind a leading "1," for the hardware type).
func matchLeaseIP(raw, wantHex string) string {
	for _, block := range strings.Split(raw, "}") {
		var ip, hw string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "ip_address="):
				ip = strings.TrimPrefix(line, "ip_address=")
			case strings.HasPrefix(line, "hw_address="):
				hw = strings.TrimPrefix(line, "hw_address=")
			}
		}
		if hw == "" {
			continue
		}
		// hw_address looks like "1,2:bf:13:..." — drop the "<type>," prefix
		// and any colons.
		if i := strings.IndexByte(hw, ','); i >= 0 {
			hw = hw[i+1:]
		}
		hw = strings.ToLower(strings.ReplaceAll(hw, ":", ""))
		// Apple strips leading zeros per byte (e.g. "2:bf:13" not "02:bf:13").
		if hw == wantHex || hw == strings.TrimLeft(wantHex, "0") || strippedEq(hw, wantHex) {
			return ip
		}
	}
	return ""
}

// strippedEq compares two hex MACs after dropping every per-byte leading
// zero, since Apple's dhcpd_leases uses "2:bf:13" where we'd write "02bf13".
func strippedEq(a, b string) bool {
	return canonicalMAC(a) == canonicalMAC(b)
}
func canonicalMAC(s string) string {
	// Re-insert separators every 2 chars, then strip leading zeros per byte.
	if !strings.Contains(s, ":") && len(s)%2 == 0 {
		var sb strings.Builder
		for i := 0; i < len(s); i += 2 {
			if i > 0 {
				sb.WriteByte(':')
			}
			sb.WriteString(s[i : i+2])
		}
		s = sb.String()
	}
	out := make([]string, 0, 6)
	for _, b := range strings.Split(s, ":") {
		out = append(out, strings.TrimLeft(b, "0"))
	}
	return strings.Join(out, ":")
}

// lookupTailnetIP asks the host's tailscaled for the IP it assigned to
// `hostname`. Polls until an ONLINE device with that hostname (or a
// dedup-suffixed variant like "<hostname>-1") shows up. macOS CLI lives
// inside the GUI app bundle; on Linux it's on PATH. Either way the
// process is system-installed and bypasses the vmnet kernel policy.
func lookupTailnetIP(ctx context.Context, hostname string, timeout time.Duration) (string, error) {
	bin := tailscaleBinary()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
		if err == nil {
			if ip := pickTailnetIP(out, hostname); ip != "" {
				return ip, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("host's tailscale never saw an online device named %q within %s — check `tailscale status` and prune stale records via https://login.tailscale.com/admin/machines", hostname, timeout)
}

func tailscaleBinary() string {
	if runtime.GOOS == "darwin" {
		const appCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if _, err := os.Stat(appCLI); err == nil {
			return appCLI
		}
	}
	return "tailscale"
}

// pickTailnetIP returns the tailnet IP for the most-relevant device whose
// HostName matches `want` or `<want>-N` (tailscale's dedup suffix when an
// older registration with the same name is still around). Prefers Online
// devices; falls back to the most recently seen offline match only if no
// online candidate exists.
func pickTailnetIP(statusJSON []byte, want string) string {
	var s struct {
		Peer map[string]struct {
			HostName     string   `json:"HostName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
			LastSeen     string   `json:"LastSeen"`
		} `json:"Peer"`
	}
	if err := json.Unmarshal(statusJSON, &s); err != nil {
		return ""
	}
	var (
		onlineIP  string
		offlineIP string
		offlineAt string
	)
	for _, p := range s.Peer {
		if !hostnameMatches(p.HostName, want) || len(p.TailscaleIPs) == 0 {
			continue
		}
		ip := p.TailscaleIPs[0]
		if p.Online {
			onlineIP = ip
			break
		}
		if p.LastSeen > offlineAt {
			offlineAt = p.LastSeen
			offlineIP = ip
		}
	}
	if onlineIP != "" {
		return onlineIP
	}
	return offlineIP
}

// hostnameMatches accepts the bare hostname OR its dedup-suffixed variant
// ("<want>-N" where N is 1..). Tailscale appends -N when an older
// registration with the same name is still around.
func hostnameMatches(got, want string) bool {
	if got == want {
		return true
	}
	if !strings.HasPrefix(got, want+"-") {
		return false
	}
	suffix := got[len(want)+1:]
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return suffix != ""
}

// ensureDefaultNetworkActive is the load-bearing preflight for system-mode
// libvirt on Linux. On macOS we use vmnet directly (no `default` network
// exists, since Linux-bridge rename is unsupported), but we DO need
// virtlogd's socket — libvirt 6+ writes per-domain serial logs through it
// and refuses to start VMs without it. Brew packages virtlogd but ships no
// launchd plist, so it's a separate one-time daemonize for operators.
func ensureDefaultNetworkActive(ctx context.Context, uri string) error {
	if runtime.GOOS == "darwin" {
		const sock = "/opt/homebrew/var/run/libvirt/virtlogd-sock"
		if _, err := os.Stat(sock); err != nil {
			return fmt.Errorf(`virtlogd is not running — its socket %s is missing.

brew packages virtlogd but ships no launchd plist for it. Start it once:

  sudo /opt/homebrew/sbin/virtlogd --daemon

It persists until the next reboot.`, sock)
		}
		return nil
	}
	out, err := virsh(ctx, uri, "net-info", defaultNetwork)
	if err != nil {
		return fmt.Errorf(`libvirt's %q network is missing on %s.

Linux:   sudo virsh net-define /usr/share/libvirt/networks/default.xml
         sudo virsh net-autostart default
         sudo virsh net-start default
macOS:   sudo brew services start libvirt
         sudo virsh -c qemu:///system net-autostart default
         sudo virsh -c qemu:///system net-start default

(underlying error: %w)`, defaultNetwork, uri, err)
	}
	if !strings.Contains(string(out), "Active:") {
		return fmt.Errorf("internal: unexpected virsh net-info output: %s", string(out))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Active:") {
			if strings.Contains(line, "yes") {
				return nil
			}
			return fmt.Errorf(`libvirt's %q network is defined but inactive. Start it with:

  sudo virsh -c %s net-autostart %s
  sudo virsh -c %s net-start %s`, defaultNetwork, uri, defaultNetwork, uri, defaultNetwork)
		}
	}
	return nil
}
