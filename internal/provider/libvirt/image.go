package libvirt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// ensureBaseImage downloads the upstream Alpine cloud qcow2 once into the
// image cache and returns its path. Subsequent calls are no-ops. The cache
// is shared across clusters — every libvirt cluster on this host uses the
// same backing file via qcow2 overlays.
func (p *Provider) ensureBaseImage(ctx context.Context) (string, error) {
	u, err := url.Parse(defaultImageURL)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(p.imageDir, filepath.Base(u.Path))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	// Download to <dst>.tmp then atomic-rename so a half-finished download
	// from a previous interrupted grow doesn't get picked up.
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, defaultImageURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", defaultImageURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", defaultImageURL, resp.StatusCode)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write base image: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	if defaultImageSHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != defaultImageSHA256 {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("base image sha256 mismatch: got %s, want %s", got, defaultImageSHA256)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename: %w", err)
	}
	return dst, nil
}
