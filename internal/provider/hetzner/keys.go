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
