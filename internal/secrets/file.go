package secrets

import (
	"context"
	"os"
	"path/filepath"
)

// File store — for non-AWS providers and local development. Writes one file
// per key under a root directory.
type File struct{ Root string }

func NewFile(root string) *File { return &File{Root: root} }

func (f *File) Write(_ context.Context, key, value string) error {
	path := filepath.Join(f.Root, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value), 0o600)
}

func (f *File) Read(_ context.Context, key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(f.Root, key))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
