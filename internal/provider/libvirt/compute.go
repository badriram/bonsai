package libvirt

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bcfg "github.com/badriram/bonsai/internal/config"

	"k8s.io/apimachinery/pkg/api/resource"
)

// libvirtHeadroomGB is the OS + k3s + image-pull margin reserved on top of
// the operator's postgres.volume_size when we size the qcow2 overlay. qcow2
// is thin so the bytes only materialise on the host disk as Postgres + the
// rest of the VM actually fill them — pick a generous number that keeps
// homelab clusters from running into ENOSPC while keeping default host disk
// usage near zero.
const libvirtHeadroomGB = 20

// provisionedDiskGB derives the qcow2 virtual size from the declared Postgres
// volume size + a fixed OS/k3s headroom. libvirt cannot resize PVCs at
// runtime (local-path-provisioner has no expand capability), so the size
// chosen at first grow is the cluster's ceiling until destroy+grow.
func provisionedDiskGB(cfg bcfg.ClusterConfig) int64 {
	pgGiB := int64(10)
	if cfg.PostgresVolumeSize != "" {
		if q, err := resource.ParseQuantity(cfg.PostgresVolumeSize); err == nil {
			gi := q.Value() / (1 << 30)
			if q.Value()%(1<<30) != 0 {
				gi++
			}
			pgGiB = gi
		}
	}
	return pgGiB + libvirtHeadroomGB
}

// domainXML mirrors a tiny subset of libvirt's domain.xsd. We marshal Go
// structs to XML rather than templating strings so the indentation /
// escaping bug-#11 class can't sneak back in via a different door.
type domainXML struct {
	XMLName   xml.Name `xml:"domain"`
	XmlnsQemu string   `xml:"xmlns:qemu,attr,omitempty"`
	Type      string   `xml:"type,attr"`
	Name      string   `xml:"name"`
	Memory    unitVal  `xml:"memory"`
	VCPU      int      `xml:"vcpu"`
	OS        osBlock  `xml:"os"`
	Devices   devices  `xml:"devices"`
	// QemuCmd is populated on macOS where libvirt can't yet model
	// vmnet-shared via <interface>; we inject `-netdev vmnet-shared` raw.
	QemuCmd *qemuCommandline `xml:"qemu:commandline,omitempty"`
}
type qemuCommandline struct {
	Args []qemuArg `xml:"qemu:arg"`
}
type qemuArg struct {
	Value string `xml:"value,attr"`
}
type unitVal struct {
	Unit  string `xml:"unit,attr"`
	Value int    `xml:",chardata"`
}
type osBlock struct {
	// Firmware="efi" tells libvirt+QEMU to autoselect OVMF/EDK2. Mandatory on
	// aarch64 (no legacy BIOS exists); also required by Alpine's UEFI cloud
	// images on x86_64 — the `*-uefi-*` qcow2 has its boot loader stub
	// configured for EDK2 only, so SeaBIOS fails to find a bootable entry.
	Firmware string `xml:"firmware,attr,omitempty"`
	Type     osType `xml:"type"`
}
type osType struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}
type devices struct {
	Disks      []disk      `xml:"disk"`
	Interfaces []iface     `xml:"interface"`
	Console    consoleBlk  `xml:"console"`
	Channels   []channel   `xml:"channel"`
	Graphics   []graphics  `xml:"graphics"`
}
type disk struct {
	Type   string     `xml:"type,attr"`
	Device string     `xml:"device,attr"`
	Driver diskDriver `xml:"driver"`
	Source diskSource `xml:"source"`
	Target diskTarget `xml:"target"`
}
type diskDriver struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}
type diskSource struct {
	File string `xml:"file,attr"`
}
type diskTarget struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}
type iface struct {
	Type   string     `xml:"type,attr"`
	MAC    *ifaceMAC  `xml:"mac,omitempty"`
	Source ifaceSrc   `xml:"source"`
	Model  ifaceModel `xml:"model"`
}
type ifaceMAC struct {
	Address string `xml:"address,attr"`
}
type ifaceSrc struct {
	// Network is set on Linux (`<source network='default'/>`); Mode is set on
	// macOS where we use `<interface type='vmnet'><source mode='shared'/>`
	// because libvirt's default Linux-bridge network can't exist on Darwin
	// (if_bridge doesn't support interface rename to virbr0).
	Network string `xml:"network,attr,omitempty"`
	Mode    string `xml:"mode,attr,omitempty"`
}
type ifaceModel struct {
	Type string `xml:"type,attr"`
}
type consoleBlk struct {
	Type string `xml:"type,attr"`
}
type channel struct {
	Type   string        `xml:"type,attr"`
	Target channelTarget `xml:"target"`
}
type channelTarget struct {
	Type string `xml:"type,attr"`
	Name string `xml:"name,attr"`
}
type graphics struct {
	Type   string `xml:"type,attr"`
	Listen string `xml:"listen,attr,omitempty"`
}

// createControlVM provisions a single-node control plane. Returns the
// domain UUID and the discovered IP.
func (p *Provider) createControlVM(ctx context.Context, cfg bcfg.ClusterConfig, baseImage string, key *sshKeyPair) (string, string, error) {
	name := fmt.Sprintf("bonsai-%s-%s-control", cfg.Name, cfg.Env)

	script, err := renderServerScript(serverVars{K3sVersion: k3sVersionOrDefault(cfg.K3sVersion)})
	if err != nil {
		return "", "", err
	}
	uuid, ip, err := p.createVM(ctx, cfg, name, baseImage, script, key)
	if err != nil {
		return "", "", err
	}
	return uuid, ip, nil
}

// ensureWorkers creates worker VMs up to `desired`. Workers join via the
// control plane's IP (controlIP) and use the join token Bonsai pulled.
func (p *Provider) ensureWorkers(ctx context.Context, cfg bcfg.ClusterConfig, baseImage string, key *sshKeyPair, controlIP, token string, desired int) error {
	for i := 0; i < desired; i++ {
		script, err := renderWorkerScript(workerVars{
			K3sVersion: k3sVersionOrDefault(cfg.K3sVersion),
			ControlIP:  controlIP,
			Token:      token,
		})
		if err != nil {
			return err
		}
		name := fmt.Sprintf("bonsai-%s-%s-worker-%d", cfg.Name, cfg.Env, i+1)
		if _, _, err := p.createVM(ctx, cfg, name, baseImage, script, key); err != nil {
			return fmt.Errorf("worker %d: %w", i+1, err)
		}
	}
	return nil
}

// createVM is the shared body for control + worker VM creation: overlay
// disk → NoCloud ISO → domain XML → define + start → wait for IP.
func (p *Provider) createVM(ctx context.Context, cfg bcfg.ClusterConfig, name, baseImage, userDataScript string, key *sshKeyPair) (string, string, error) {
	pool := filepath.Join(p.dataDir, cfg.Name+"-"+cfg.Env, "vms")
	if err := os.MkdirAll(pool, 0o700); err != nil {
		return "", "", err
	}
	overlay := filepath.Join(pool, name+".qcow2")
	diskGB := provisionedDiskGB(cfg)
	if _, err := qemuImg(ctx, "create", "-F", "qcow2", "-b", baseImage, "-f", "qcow2", overlay, fmt.Sprintf("%dG", diskGB)); err != nil {
		return "", "", fmt.Errorf("qemu-img create overlay: %w", err)
	}
	isoPath := filepath.Join(pool, name+"-seed.iso")
	if err := buildNoCloudISO(ctx, isoPath, name, userDataScript, key.PublicOpenSSH); err != nil {
		return "", "", fmt.Errorf("buildNoCloudISO: %w", err)
	}

	arch, machine := libvirtArch()
	d := domainXML{
		Type:   defaultDomainType(),
		Name:   name,
		Memory: unitVal{Unit: "MiB", Value: defaultMemoryMB},
		VCPU:   defaultVCPUs,
		OS: osBlock{
			Firmware: "efi",
			Type:     osType{Arch: arch, Machine: machine, Value: "hvm"},
		},
		Devices: devices{
			Disks: []disk{
				{
					Type:   "file",
					Device: "disk",
					Driver: diskDriver{Name: "qemu", Type: "qcow2"},
					Source: diskSource{File: overlay},
					Target: diskTarget{Dev: "vda", Bus: "virtio"},
				},
				{
					Type:   "file",
					Device: "cdrom",
					Driver: diskDriver{Name: "qemu", Type: "raw"},
					Source: diskSource{File: isoPath},
					Target: diskTarget{Dev: "sda", Bus: "sata"},
				},
			},
			Interfaces: guestInterfaces(name),
			Console: consoleBlk{Type: "pty"},
			Channels: []channel{
				{Type: "unix", Target: channelTarget{Type: "virtio", Name: "org.qemu.guest_agent.0"}},
			},
			Graphics: []graphics{{Type: "vnc", Listen: "127.0.0.1"}},
		},
	}
	if args := guestQemuArgs(name); len(args) > 0 {
		d.XmlnsQemu = "http://libvirt.org/schemas/domain/qemu/1.0"
		d.QemuCmd = &qemuCommandline{Args: args}
	}
	xmlBytes, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", "", err
	}

	xmlPath := filepath.Join(pool, name+".xml")
	if err := os.WriteFile(xmlPath, xmlBytes, 0o600); err != nil {
		return "", "", err
	}
	if _, err := virsh(ctx, p.uri, "define", xmlPath); err != nil {
		return "", "", fmt.Errorf("virsh define: %w", err)
	}
	if _, err := virsh(ctx, p.uri, "start", name); err != nil {
		return "", "", fmt.Errorf("virsh start: %w", err)
	}

	uuidOut, err := virsh(ctx, p.uri, "domuuid", name)
	if err != nil {
		return "", "", fmt.Errorf("virsh domuuid: %w", err)
	}
	uuid := strings.TrimSpace(string(uuidOut))

	ip, err := p.waitForGuestIP(ctx, name, sshReadyTimeout)
	if err != nil {
		return uuid, "", err
	}
	return uuid, ip, nil
}

// waitForVMIP polls `virsh domifaddr <name>` until the lease appears.
// virsh's default source is "lease" which works for libvirt's default NAT
// network; for bridged networks an operator may need --source agent.
func (p *Provider) waitForGuestIP(ctx context.Context, name string, timeout time.Duration) (string, error) {
	// macOS: VM is on Apple's vmnet shared subnet (192.168.64.0/24); leases
	// land in /var/db/dhcpd_leases keyed by MAC, not in libvirt's database.
	if runtime.GOOS == "darwin" {
		return lookupVMNetIP(ctx, deterministicMAC(name), timeout)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := virsh(ctx, p.uri, "domifaddr", name)
		if err == nil {
			if ip := parseFirstIPv4(string(out)); ip != "" {
				return ip, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", fmt.Errorf("VM %s never reported an IPv4 lease within %s", name, timeout)
}

// parseFirstIPv4 extracts the first IPv4 address from `virsh domifaddr`
// table output. Columns are: Name, MAC, Protocol, Address.
func parseFirstIPv4(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[2] != "ipv4" {
			continue
		}
		addr := fields[3]
		if i := strings.Index(addr, "/"); i > 0 {
			addr = addr[:i]
		}
		return addr
	}
	return ""
}

func k3sVersionOrDefault(v string) string {
	if v != "" {
		return v
	}
	return defaultK3sVersion
}
