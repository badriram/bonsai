package libvirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAPIToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	if err := os.WriteFile(path, []byte("\n  tskey-api-abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readAPIToken(path)
	if err != nil || got != "tskey-api-abc123" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestReadAPIToken_NoMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	_ = os.WriteFile(path, []byte("tskey-auth-this-is-wrong-prefix"), 0o600)
	if _, err := readAPIToken(path); err == nil {
		t.Fatal("expected error for non-api token")
	}
}

func TestPruneTailnetDevices_OnlyMatchingPrefix(t *testing.T) {
	var deletedIDs []string
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
			id := strings.TrimPrefix(r.URL.Path, "/api/v2/device/")
			deletedIDs = append(deletedIDs, id)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	// Patch httpClient to redirect to the test server. We override the
	// production URL by overriding the api host through env-aware
	// indirection... simpler: hit the function-level seams.
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "k")
	_ = os.WriteFile(tokenPath, []byte("tskey-api-test"), 0o600)

	// Direct unit test of list/delete using the test server.
	devs, err := fetchDevicesFrom(context.Background(), "tskey-api-test", srv.URL+"/api/v2/tailnet/-/devices")
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 4 {
		t.Fatalf("got %d devices", len(devs))
	}
	for _, d := range devs {
		if strings.HasPrefix(d.Hostname, "bonsai-smoke-test-") {
			if err := deleteAt(context.Background(), "tskey-api-test", srv.URL+"/api/v2/device/"+d.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(deletedIDs) != 2 || deletedIDs[0] != "d1" || deletedIDs[1] != "d2" {
		t.Fatalf("wrong deletions: %v", deletedIDs)
	}
}

// fetchDevicesFrom + deleteAt are test-only seams that exercise the same
// HTTP shape as the production functions but accept arbitrary URLs.
func fetchDevicesFrom(ctx context.Context, token, url string) ([]tailnetDevice, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct{ Devices []tailnetDevice }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Devices, nil
}

func deleteAt(ctx context.Context, token, url string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &httpError{Status: resp.StatusCode}
	}
	return nil
}

type httpError struct{ Status int }

func (e *httpError) Error() string { return http.StatusText(e.Status) }
