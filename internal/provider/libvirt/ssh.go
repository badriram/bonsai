package libvirt

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/badriram/bonsai/internal/secrets"
)

const sshPubKeySecret = "ssh_public_key"

// sshKeyPair is the on-disk client key Bonsai uses to SSH into VMs. Stored
// in the secret store next to host keys, mirrors the Hetzner provider.
type sshKeyPair struct {
	PrivateOpenSSH string
	PublicOpenSSH  string // single line, authorized_keys format
}

func (p *Provider) ensureSSHKey(ctx context.Context, name, env string) (*sshKeyPair, error) {
	priv, err := p.store.Read(ctx, secrets.LocalKey(name, env, sshPrivateKeyKey))
	pub, _ := p.store.Read(ctx, secrets.LocalKey(name, env, sshPubKeySecret))
	if err == nil && priv != "" && pub != "" {
		return &sshKeyPair{PrivateOpenSSH: priv, PublicOpenSSH: pub}, nil
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
	return &sshKeyPair{PrivateOpenSSH: pemStr, PublicOpenSSH: pubLine}, nil
}

// sshClient dials a VM. host-key TOFU for V1; pinning lands when bake
// pre-seeds a per-cluster host key the way Hetzner does (parity follow-up).
func (p *Provider) sshClient(addr string, key *sshKeyPair) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey([]byte(key.PrivateOpenSSH))
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	return ssh.Dial("tcp", addr+":22", cfg)
}

func runSSH(client *ssh.Client, cmd string) (string, error) {
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
func (p *Provider) waitForReady(ctx context.Context, ip string, key *sshKeyPair, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client, err := p.sshClient(ip, key)
		if err == nil {
			out, err := runSSH(client, "test -f /var/lib/bonsai-server-ready && echo ok")
			client.Close()
			if err == nil && strings.TrimSpace(out) == "ok" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("VM %s never reached ready marker within %s", ip, timeout)
}

// retrieveControlState pulls the k3s join token + kubeconfig over SSH. The
// kubeconfig's 127.0.0.1 is rewritten to the VM's reachable IP.
func (p *Provider) retrieveControlState(ctx context.Context, ip string, key *sshKeyPair) (token, kubeconfig string, err error) {
	_ = ctx
	client, err := p.sshClient(ip, key)
	if err != nil {
		return "", "", err
	}
	defer client.Close()
	tok, err := runSSH(client, "cat /var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return "", "", fmt.Errorf("read token: %w", err)
	}
	kc, err := runSSH(client, "cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return "", "", fmt.Errorf("read kubeconfig: %w", err)
	}
	kc = strings.ReplaceAll(kc, "127.0.0.1", ip)
	return strings.TrimSpace(tok), kc, nil
}
