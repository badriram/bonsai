package secrets

import (
	"context"
	"fmt"
	"time"
)

// Cached layers a fast local cache (File) over a remote source of truth. The
// remote always wins: reads pull from remote and refresh the cache as a
// side-effect; writes hit remote first and only update the cache after the
// remote write succeeds. If the remote is unreachable, operations fail —
// callers should not get a stale cache silently.
//
// remoteToLocal translates a caller-supplied remote key (e.g. an absolute SSM
// path like "/bonsai/foo/prod/kubeconfig") into the path used inside the local
// cache (e.g. "foo-prod/kubeconfig"). This keeps the on-disk layout uniform
// across providers regardless of how the remote namespaces its keys. Identity
// when nil.
//
// remoteRef is an optional formatter that produces a human-readable pointer
// (e.g. "ssm:///bonsai/foo/prod/kubeconfig") recorded in the cache sidecar so
// `bonsai status` can show where the canonical copy lives and when this
// machine last synced.
type Cached struct {
	remote        Store
	cache         *File
	remoteToLocal func(string) string
	remoteRef     func(string) string
}

func NewCached(remote Store, cache *File, remoteToLocal, remoteRef func(string) string) *Cached {
	if remoteToLocal == nil {
		remoteToLocal = func(k string) string { return k }
	}
	if remoteRef == nil {
		remoteRef = func(k string) string { return k }
	}
	return &Cached{remote: remote, cache: cache, remoteToLocal: remoteToLocal, remoteRef: remoteRef}
}

func (c *Cached) Write(ctx context.Context, key, value string) error {
	if err := c.remote.Write(ctx, key, value); err != nil {
		return fmt.Errorf("remote write %s: %w", key, err)
	}
	meta := Metadata{Remote: c.remoteRef(key), RefreshedAt: time.Now().UTC()}
	if err := c.cache.WriteWithMeta(ctx, c.remoteToLocal(key), value, meta); err != nil {
		return fmt.Errorf("cache write-through %s: %w", key, err)
	}
	return nil
}

func (c *Cached) Read(ctx context.Context, key string) (string, error) {
	val, err := c.remote.Read(ctx, key)
	if err != nil {
		return "", fmt.Errorf("remote read %s: %w", key, err)
	}
	meta := Metadata{Remote: c.remoteRef(key), RefreshedAt: time.Now().UTC()}
	_ = c.cache.WriteWithMeta(ctx, c.remoteToLocal(key), val, meta)
	return val, nil
}
