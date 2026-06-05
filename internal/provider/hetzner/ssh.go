package hetzner

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/crypto/ssh"

	"github.com/badriram/bonsai/internal/secrets"
)

const (
	sshLocalKeyName    = "ssh_private_key"
	sshHostKeyName     = "ssh_host_key"      // Bonsai-controlled host private key (PEM)
	sshHostPubName     = "ssh_host_key_pub"  // its authorized-key form (one line)
)

// ensureSSHKey returns a Bonsai-owned SSH key pair for the cluster, creating
// one on first call. The client private key goes to the FileSecretStore and the
// client public key is uploaded to Hetzner. A second, Bonsai-controlled SSH
// HOST key is also generated and stored locally — server.sh.tmpl + restore.sh.tmpl
// inject it via cloud-init's ssh_keys: block so sshd starts with a key Bonsai
// knows, and sshClient pins against it with ssh.FixedHostKey. This is what
// prevents on-path MITM during initial provision + rotation.
func (p *Provider) ensureSSHKey(ctx context.Context, name, env string) (*hcloud.SSHKey, error) {
	keys, err := p.client.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "ssh-key")},
	})
	if err != nil {
		return nil, err
	}
	// Always ensure a host key exists locally — clusters provisioned before
	// the host-key feature won't have one and need a top-up before the next
	// rotate-control can pin against it.
	if err := p.ensureHostKey(ctx, name, env); err != nil {
		return nil, fmt.Errorf("ensure host key: %w", err)
	}
	if len(keys) > 0 {
		return keys[0], nil
	}

	authorized, privPEM, err := generateAuthorizedKey()
	if err != nil {
		return nil, err
	}
	if err := p.store.Write(ctx, secrets.LocalKey(name, env, sshLocalKeyName), string(privPEM)); err != nil {
		return nil, fmt.Errorf("save private key: %w", err)
	}
	key, _, err := p.client.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      "bonsai-" + name + "-" + env,
		PublicKey: authorized,
		Labels:    clusterLabels(name, env, "ssh-key"),
	})
	if err != nil {
		return nil, fmt.Errorf("upload to hetzner: %w", err)
	}
	return key, nil
}

// ensureHostKey is idempotent: returns immediately if a host key is already
// stored for the cluster, otherwise generates one and writes both halves.
func (p *Provider) ensureHostKey(ctx context.Context, name, env string) error {
	if _, err := p.store.Read(ctx, secrets.LocalKey(name, env, sshHostKeyName)); err == nil {
		return nil
	}
	authorized, privPEM, err := generateHostKey()
	if err != nil {
		return err
	}
	if err := p.store.Write(ctx, secrets.LocalKey(name, env, sshHostKeyName), string(privPEM)); err != nil {
		return fmt.Errorf("save host private key: %w", err)
	}
	if err := p.store.Write(ctx, secrets.LocalKey(name, env, sshHostPubName), authorized); err != nil {
		return fmt.Errorf("save host public key: %w", err)
	}
	return nil
}

// hostKeyMaterial returns the cluster's host key in the two forms cloud-init
// + cloud-config templates need.
func (p *Provider) hostKeyMaterial(ctx context.Context, name, env string) (privPEM, authorizedPub string, err error) {
	priv, err := p.store.Read(ctx, secrets.LocalKey(name, env, sshHostKeyName))
	if err != nil {
		return "", "", fmt.Errorf("read host private key: %w", err)
	}
	pub, err := p.store.Read(ctx, secrets.LocalKey(name, env, sshHostPubName))
	if err != nil {
		return "", "", fmt.Errorf("read host public key: %w", err)
	}
	return priv, pub, nil
}

// sshClient connects to a Hetzner server using the cluster's stored client
// private key, and pins the server's host key to the Bonsai-controlled one
// installed via cloud-init. Refuses to connect (rather than silently falling
// back to TOFU) if no host key is stored — an unknown host key means we have
// no anchor to authenticate the server, which is the exact MITM gap the
// previous InsecureIgnoreHostKey enabled.
func (p *Provider) sshClient(ctx context.Context, name, env, ip string) (*ssh.Client, error) {
	privPEM, err := p.store.Read(ctx, secrets.LocalKey(name, env, sshLocalKeyName))
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(privPEM))
	if err != nil {
		return nil, err
	}
	hostPubLine, err := p.store.Read(ctx, secrets.LocalKey(name, env, sshHostPubName))
	if err != nil {
		return nil, fmt.Errorf("read host public key (cluster may predate host-key fix — destroy + grow to migrate): %w", err)
	}
	hostPub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostPubLine))
	if err != nil {
		return nil, fmt.Errorf("parse stored host public key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(hostPub),
		Timeout:         10 * time.Second,
	}
	return ssh.Dial("tcp", net.JoinHostPort(ip, "22"), cfg)
}

// runSSH executes a single command and returns combined stdout/stderr.
func runSSH(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// pullFile copies remotePath from the server to localPath via `cat` over an
// SSH session. Good enough for ~100MB state tarballs without a dedicated SFTP
// dep. Returns the byte count moved.
func pullFile(client *ssh.Client, remotePath, localPath string) (int64, error) {
	f, err := os.Create(localPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sess, err := client.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := sess.Start("cat " + remotePath); err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, stdout)
	if err := sess.Wait(); err != nil {
		return n, err
	}
	return n, copyErr
}

// pushFile copies localPath onto the server at remotePath the same way.
func pushFile(client *ssh.Client, localPath, remotePath string) (int64, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return 0, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := sess.Start("cat > " + remotePath); err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(stdin, f)
	stdin.Close()
	if err := sess.Wait(); err != nil {
		return n, err
	}
	if copyErr != nil {
		return n, copyErr
	}
	return stat.Size(), nil
}

// waitForSSH polls until a TCP connect to :22 succeeds. Servers take ~30s to
// come up; cloud-init can extend this.
func waitForSSH(ctx context.Context, ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), 5*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("ssh on %s not reachable within %s", ip, timeout)
}

// rewriteKubeconfig replaces the loopback server URL with the floating IP so
// the kubeconfig works from outside the server.
func rewriteKubeconfig(raw, ip string) string {
	return strings.ReplaceAll(raw, "127.0.0.1", ip)
}
