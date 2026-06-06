package libvirt

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/badriram/bonsai/internal/secrets"
)

const sshPubKeySecret = "ssh_public_key"

// sshKeyPair is the on-disk client key Bonsai uses to SSH into VMs. Stored
// in the secret store next to host keys, mirrors the Hetzner provider.
// KeyPath is the absolute on-disk path to the PEM file — used on macOS
// where we shell out to /usr/bin/ssh (Apple-signed, bypasses the kernel's
// vmnet network policy) instead of dialing via Go's net stack.
type sshKeyPair struct {
	PrivateOpenSSH string
	PublicOpenSSH  string // single line, authorized_keys format
	KeyPath        string
}

func (p *Provider) ensureSSHKey(ctx context.Context, name, env string) (*sshKeyPair, error) {
	keyPath := filepath.Join(p.dataDir, secrets.LocalKey(name, env, sshPrivateKeyKey))
	priv, err := p.store.Read(ctx, secrets.LocalKey(name, env, sshPrivateKeyKey))
	pub, _ := p.store.Read(ctx, secrets.LocalKey(name, env, sshPubKeySecret))
	if err == nil && priv != "" && pub != "" {
		return &sshKeyPair{PrivateOpenSSH: priv, PublicOpenSSH: pub, KeyPath: keyPath}, nil
	}
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ed25519: %w", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(privKey, "bonsai")
	if err != nil {
		return nil, fmt.Errorf("marshal private: %w", err)
	}
	pemStr := string(pem.EncodeToMemory(pemBlock))
	authPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, err
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(authPub)))
	if err := p.store.Write(ctx, secrets.LocalKey(name, env, sshPrivateKeyKey), pemStr); err != nil {
		return nil, err
	}
	if err := p.store.Write(ctx, secrets.LocalKey(name, env, sshPubKeySecret), pubLine); err != nil {
		return nil, err
	}
	return &sshKeyPair{PrivateOpenSSH: pemStr, PublicOpenSSH: pubLine, KeyPath: keyPath}, nil
}

// runRemoteCmd runs cmd on the guest as `alpine` and returns combined
// stdout. On macOS we shell out to /usr/bin/ssh because the kernel
// rejects connect() syscalls from non-Apple-signed binaries to
// vmnet-bridged destinations — Go's ssh.Dial fails identically to
// raw net.DialTimeout. ssh is Apple-signed and bypasses the policy.
// On Linux we use Go's ssh client directly.
//
// Callers prefix `doas` to commands that need root; Alpine's cloud
// image puts the `alpine` user in wheel with nopasswd doas, so the
// prefix Just Works.
func (p *Provider) runRemoteCmd(ctx context.Context, ip string, key *sshKeyPair, cmd string) (string, error) {
	if runtime.GOOS == "darwin" {
		return runRemoteCmdExec(ctx, ip, key.KeyPath, cmd)
	}
	return runRemoteCmdGo(ip, key, cmd)
}

func runRemoteCmdExec(ctx context.Context, ip, keyPath, cmd string) (string, error) {
	sshCmd := exec.CommandContext(ctx, "/usr/bin/ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "LogLevel=ERROR",
		"alpine@"+ip,
		cmd,
	)
	var stdout, stderr bytes.Buffer
	sshCmd.Stdout = &stdout
	sshCmd.Stderr = &stderr
	if err := sshCmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// tunnelKubeconfigDarwin opens an SSH local-forward from a free 127.0.0.1
// port to the guest's k3s API at 127.0.0.1:6443, then rewrites the
// kubeconfig's server URL to point at the local end of the tunnel. The
// in-cluster bootstrap's helm + client-go calls then dial loopback,
// sidestepping macOS's kernel restriction on non-Apple-signed binaries
// dialing vmnet-bridged destinations. The returned cleanup kills the
// tunnel.
//
// k3s's TLS cert covers 127.0.0.1 because server.sh.tmpl adds it to the
// tls-san list — verifying the cert against `https://127.0.0.1:<port>`
// works without bypassing TLS.
func tunnelKubeconfigDarwin(ctx context.Context, guestIP string, key *sshKeyPair, kubeconfig string) (func(), string, error) {
	port, err := pickFreeLoopbackPort()
	if err != nil {
		return func() {}, "", err
	}
	cleanup, err := openLoopbackTunnel(ctx, guestIP, key, port, 6443)
	if err != nil {
		return func() {}, "", err
	}
	rewritten := strings.ReplaceAll(kubeconfig, "https://"+guestIP+":6443", fmt.Sprintf("https://127.0.0.1:%d", port))
	if rewritten == kubeconfig {
		rewritten = strings.ReplaceAll(kubeconfig, "127.0.0.1:6443", fmt.Sprintf("127.0.0.1:%d", port))
	}
	return cleanup, rewritten, nil
}

// pickFreeLoopbackPort asks the kernel for any free 127.0.0.1 port by
// listening with :0 and reading the chosen port back out. There's a small
// race between Close and ssh re-binding, but ssh will fail loudly if it
// can't get the port and we'll surface that.
func pickFreeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// openLoopbackTunnel spawns `/usr/bin/ssh -L <localPort>:127.0.0.1:<remotePort>` in
// the background. Apple-signed ssh sets up the forward; Go callers then
// dial 127.0.0.1:<localPort> instead of the vmnet-bridged guest IP — which
// macOS's kernel network policy would otherwise block for non-Apple-signed
// binaries. Loopback connects are unrestricted. Returns a cleanup that
// kills the tunnel; safe to call multiple times.
func openLoopbackTunnel(ctx context.Context, ip string, key *sshKeyPair, localPort, remotePort int) (func(), error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh",
		"-i", key.KeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-N",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remotePort),
		"alpine@"+ip,
	)
	if err := cmd.Start(); err != nil {
		return func() {}, fmt.Errorf("start ssh tunnel: %w", err)
	}
	// Wait for the forward to be accepting on the local port. ssh writes
	// nothing on success with -N, so poll a TCP connect.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if reachableTCP(fmt.Sprintf("127.0.0.1:%d", localPort), 500*time.Millisecond) {
			return func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return func() {}, fmt.Errorf("ssh tunnel never started accepting on 127.0.0.1:%d", localPort)
}

func runRemoteCmdGo(ip string, key *sshKeyPair, cmd string) (string, error) {
	signer, err := ssh.ParsePrivateKey([]byte(key.PrivateOpenSSH))
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            "alpine",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	client, err := ssh.Dial("tcp", ip+":22", cfg)
	if err != nil {
		return "", err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// waitForReady polls /var/lib/bonsai-server-ready over SSH. The marker is
// touched by the first-boot script after `rc-service k3s-server start`.
// /var/lib is world-readable so no doas prefix needed.
func (p *Provider) waitForReady(ctx context.Context, ip string, key *sshKeyPair, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := p.runRemoteCmd(ctx, ip, key, "test -f /var/lib/bonsai-server-ready && echo ok")
		if err == nil && strings.TrimSpace(out) == "ok" {
			return nil
		}
		lastErr = fmt.Errorf("out=%q err=%w", strings.TrimSpace(out), err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("VM %s never reached ready marker within %s (last error: %v)", ip, timeout, lastErr)
}

// retrieveControlState pulls the k3s join token + kubeconfig over SSH. The
// kubeconfig's 127.0.0.1 is rewritten to the VM's reachable IP.
func (p *Provider) retrieveControlState(ctx context.Context, ip string, key *sshKeyPair) (token, kubeconfig string, err error) {
	tok, err := p.runRemoteCmd(ctx, ip, key, "doas cat /var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return "", "", fmt.Errorf("read token: %w", err)
	}
	kc, err := p.runRemoteCmd(ctx, ip, key, "doas cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return "", "", fmt.Errorf("read kubeconfig: %w", err)
	}
	kc = strings.ReplaceAll(kc, "127.0.0.1", ip)
	return strings.TrimSpace(tok), kc, nil
}
