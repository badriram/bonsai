package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type fakeStore struct {
	data    map[string]string
	writeFn func(string, string) error
	readFn  func(string) (string, error)
}

func (f *fakeStore) Write(_ context.Context, k, v string) error {
	if f.writeFn != nil {
		return f.writeFn(k, v)
	}
	f.data[k] = v
	return nil
}
func (f *fakeStore) Read(_ context.Context, k string) (string, error) {
	if f.readFn != nil {
		return f.readFn(k)
	}
	v, ok := f.data[k]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func TestCachedWritesRemoteFirstThenCache(t *testing.T) {
	dir := t.TempDir()
	remote := &fakeStore{data: map[string]string{}}
	cache := NewFile(dir)
	c := NewCached(remote, cache, nil, func(k string) string { return "ssm://" + k })

	if err := c.Write(context.Background(), "/foo/kubeconfig", "abc"); err != nil {
		t.Fatal(err)
	}
	if remote.data["/foo/kubeconfig"] != "abc" {
		t.Fatalf("remote not written: %v", remote.data)
	}
	got, err := cache.Read(context.Background(), "/foo/kubeconfig")
	if err != nil || got != "abc" {
		t.Fatalf("cache miss: %q %v", got, err)
	}
	meta, err := cache.ReadMeta(context.Background(), "/foo/kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Remote != "ssm:///foo/kubeconfig" {
		t.Fatalf("meta.Remote = %q", meta.Remote)
	}
	if meta.RefreshedAt.IsZero() {
		t.Fatal("meta.RefreshedAt not set")
	}
}

func TestCachedWriteFailsIfRemoteFails(t *testing.T) {
	dir := t.TempDir()
	remote := &fakeStore{data: map[string]string{}, writeFn: func(string, string) error {
		return errors.New("ssm offline")
	}}
	cache := NewFile(dir)
	c := NewCached(remote, cache, nil, nil)

	if err := c.Write(context.Background(), "/foo/k", "v"); err == nil {
		t.Fatal("expected error when remote write fails")
	}
	if _, err := cache.Read(context.Background(), "/foo/k"); err == nil {
		t.Fatal("cache must not be written when remote write fails")
	}
}

func TestCachedReadAlwaysHitsRemoteAndRefreshesCache(t *testing.T) {
	dir := t.TempDir()
	remote := &fakeStore{data: map[string]string{"/foo/k": "fresh"}}
	cache := NewFile(dir)
	if err := cache.Write(context.Background(), "/foo/k", "stale"); err != nil {
		t.Fatal(err)
	}
	c := NewCached(remote, cache, nil, nil)
	got, err := c.Read(context.Background(), "/foo/k")
	if err != nil || got != "fresh" {
		t.Fatalf("got %q err=%v; want fresh", got, err)
	}
	cached, _ := cache.Read(context.Background(), "/foo/k")
	if cached != "fresh" {
		t.Fatalf("cache not refreshed: %q", cached)
	}
}

func TestCachedReadFailsIfRemoteFails(t *testing.T) {
	dir := t.TempDir()
	remote := &fakeStore{readFn: func(string) (string, error) { return "", errors.New("ssm offline") }}
	cache := NewFile(dir)
	_ = cache.Write(context.Background(), "/foo/k", "stale")
	c := NewCached(remote, cache, nil, nil)
	if _, err := c.Read(context.Background(), "/foo/k"); err == nil {
		t.Fatal("expected read error; cache must not silently serve stale")
	}
}

func TestCachedUsesNormalizedLocalKey(t *testing.T) {
	dir := t.TempDir()
	remote := &fakeStore{data: map[string]string{}}
	cache := NewFile(dir)
	c := NewCached(remote, cache, SSMToLocal, func(k string) string { return "ssm://" + k })

	if err := c.Write(context.Background(), "/bonsai/my-app/prod/kubeconfig", "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Read(context.Background(), "my-app-prod/kubeconfig"); err != nil {
		t.Fatalf("cache file not at canonical path: %v", err)
	}
	if _, err := cache.Read(context.Background(), "/bonsai/my-app/prod/kubeconfig"); err == nil {
		t.Fatal("cache should not be written at the SSM-shaped path")
	}
}

func TestSSMToLocal(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/bonsai/foo/prod/kubeconfig", "foo-prod/kubeconfig"},
		{"/bonsai/my-app/dev/postgres_url", "my-app-dev/postgres_url"},
		{"/something/else", "/something/else"},
		{"/bonsai/onlytwo", "/bonsai/onlytwo"},
	}
	for _, tc := range cases {
		if got := SSMToLocal(tc.in); got != tc.want {
			t.Errorf("SSMToLocal(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFileWriteCreatesSidecar(t *testing.T) {
	dir := t.TempDir()
	f := NewFile(dir)
	if err := f.Write(context.Background(), "kubeconfig", "kc"); err != nil {
		t.Fatal(err)
	}
	meta, err := f.ReadMeta(context.Background(), "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	if meta.RefreshedAt.IsZero() {
		t.Fatal("RefreshedAt zero")
	}
	if meta.Remote != "" {
		t.Fatalf("Remote should be empty for direct File writes, got %q", meta.Remote)
	}
	if _, err := filepath.Abs(filepath.Join(dir, "kubeconfig.meta.json")); err != nil {
		t.Fatal(err)
	}
}
