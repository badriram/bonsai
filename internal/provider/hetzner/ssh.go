package hetzner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/crypto/ssh"
)

const sshLocalKeyName = "ssh_private_key"

// ensureSSHKey returns a Bonsai-owned SSH key pair for the cluster, creating
// one on first call. Private key goes to the FileSecretStore; public key is
// uploaded to Hetzner.
func (p *Provider) ensureSSHKey(ctx context.Context, name, env string) (*hcloud.SSHKey, error) {
	keys, err := p.client.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "ssh-key")},
	})
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		return keys[0], nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	authorized := ssh.MarshalAuthorizedKey(sshPub)

	privBytes, err := ssh.MarshalPrivateKey(priv, "bonsai/"+name+"/"+env)
	if err != nil {
		return nil, err
	}
	privPEM := pem.EncodeToMemory(privBytes)

	if err := p.store.Write(ctx, secretKey(name, env, sshLocalKeyName), string(privPEM)); err != nil {
		return nil, fmt.Errorf("save private key: %w", err)
	}

	key, _, err := p.client.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      "bonsai-" + name + "-" + env,
		PublicKey: string(authorized),
		Labels:    clusterLabels(name, env, "ssh-key"),
	})
	if err != nil {
		return nil, fmt.Errorf("upload to hetzner: %w", err)
	}
	return key, nil
}

// sshClient connects to a Hetzner server using the cluster's stored private key.
func (p *Provider) sshClient(ctx context.Context, name, env, ip string) (*ssh.Client, error) {
	privPEM, err := p.store.Read(ctx, secretKey(name, env, sshLocalKeyName))
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(privPEM))
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // first-boot servers, no known_hosts yet
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

// secretKey returns the FileSecretStore key path for cluster-scoped secrets.
func secretKey(name, env, key string) string {
	return name + "-" + env + "/" + key
}
