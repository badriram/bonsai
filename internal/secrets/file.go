package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// File store — primary SoT for providers without a remote secret backend
// (Hetzner, libvirt), and the local cache layer behind Cached on AWS.
//
// Layout under Root:
//
//	<key>            value bytes
//	<key>.meta.json  sidecar describing where the canonical copy lives
type File struct{ Root string }

func NewFile(root string) *File { return &File{Root: root} }

// Metadata is the sidecar JSON written next to every cached value. Remote is
// empty when the File store is itself the source of truth.
type Metadata struct {
	Remote      string    `json:"remote"`
	RefreshedAt time.Time `json:"refreshed_at"`
}

func (f *File) Write(ctx context.Context, key, value string) error {
	return f.WriteWithMeta(ctx, key, value, Metadata{RefreshedAt: time.Now().UTC()})
}

func (f *File) WriteWithMeta(_ context.Context, key, value string, meta Metadata) error {
	path := filepath.Join(f.Root, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return err
	}
	if meta.RefreshedAt.IsZero() {
		meta.RefreshedAt = time.Now().UTC()
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path+".meta.json", b, 0o600)
}

func (f *File) Read(_ context.Context, key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(f.Root, key))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (f *File) ReadMeta(_ context.Context, key string) (Metadata, error) {
	b, err := os.ReadFile(filepath.Join(f.Root, key+".meta.json"))
	if err != nil {
		return Metadata{}, err
	}
	var m Metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return Metadata{}, fmt.Errorf("parse %s.meta.json: %w", key, err)
	}
	return m, nil
}

// DefaultDataDir resolves $BONSAI_DATA_DIR, falling back to ~/.bonsai.
func DefaultDataDir() (string, error) {
	if v := os.Getenv("BONSAI_DATA_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("could not resolve home directory; set BONSAI_DATA_DIR")
	}
	return filepath.Join(home, ".bonsai"), nil
}
