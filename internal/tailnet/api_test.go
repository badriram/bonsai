package tailnet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReadAPITokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	if err := os.WriteFile(path, []byte("\n  tskey-api-abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAPITokenFile(path)
	if err != nil || got != "tskey-api-abc123" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestReadAPITokenFile_NoMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	_ = os.WriteFile(path, []byte("tskey-auth-this-is-wrong-prefix"), 0o600)
	if _, err := ReadAPITokenFile(path); err == nil {
		t.Fatal("expected error for non-api token")
	}
}

func TestPrunePrefix_FiltersAndDeletes(t *testing.T) {
	var (
		mu         sync.Mutex
		deletedIDs []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tskey-api-test" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"devices": []map[string]string{
					{"id": "d1", "hostname": "bonsai-smoke-test-control"},
					{"id": "d2", "hostname": "bonsai-smoke-test-worker-1"},
					{"id": "d3", "hostname": "bonsai-other-prod-control"},
					{"id": "d4", "hostname": "operator-laptop"},
				},
			})
		case http.MethodDelete:
			mu.Lock()
			deletedIDs = append(deletedIDs, strings.TrimPrefix(r.URL.Path, "/api/v2/device/"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "tskey-api-test", HTTP: srv.Client()}
	n, err := c.PrunePrefix(context.Background(), "bonsai-smoke-test-")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted count = %d, want 2", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deletedIDs) != 2 || deletedIDs[0] != "d1" || deletedIDs[1] != "d2" {
		t.Fatalf("unexpected deletions: %v", deletedIDs)
	}
}

func TestPrunePrefix_StopsOnDeleteFailure(t *testing.T) {
	var attempted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"devices": []map[string]string{
					{"id": "good", "hostname": "bonsai-x-y-control"},
					{"id": "bad", "hostname": "bonsai-x-y-worker-1"},
				},
			})
			return
		}
		attempted++
		if strings.HasSuffix(r.URL.Path, "/bad") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "tskey-api-test", HTTP: srv.Client()}
	n, err := c.PrunePrefix(context.Background(), "bonsai-x-y-")
	if err == nil {
		t.Fatal("expected error on partial failure")
	}
	if n != 1 {
		t.Fatalf("partial count = %d, want 1 (good deleted, bad failed)", n)
	}
	if attempted != 2 {
		t.Fatalf("attempted = %d, want 2", attempted)
	}
}

func TestListDevices_HTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "x", HTTP: srv.Client()}
	if _, err := c.ListDevices(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
}
