package hetzner

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"

	"golang.org/x/crypto/ssh"
)

// generateAuthorizedKey produces an ed25519 keypair and returns the
// authorized_keys form of the public key plus PEM-encoded private key.
// Shared between per-cluster SSH keys and the image-builder key.
func generateAuthorizedKey() (authorizedPublic string, privatePEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", nil, err
	}
	privBlock, err := ssh.MarshalPrivateKey(priv, "bonsai")
	if err != nil {
		return "", nil, err
	}
	return string(ssh.MarshalAuthorizedKey(sshPub)), pem.EncodeToMemory(privBlock), nil
}

// generateHostKey produces an ed25519 host keypair for the server. The
// authorized-key form of the public key is what sshd's HostKey line resolves
// to once cloud-init installs it; the PEM form is what cloud-init writes to
// /etc/ssh/ssh_host_ed25519_key. Same shape as generateAuthorizedKey, kept
// separate so future host-key-rotation logic stays orthogonal to client keys.
func generateHostKey() (authorizedPublic string, privatePEM []byte, err error) {
	return generateAuthorizedKey()
}
