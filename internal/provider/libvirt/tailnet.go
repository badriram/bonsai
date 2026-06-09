package libvirt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// pruneTailnetDevices deletes every Tailscale device whose hostname begins
// with `prefix` from the operator's tailnet. Run on destroy so the cluster
// doesn't leave dangling records behind — ephemeral keys age devices out
// eventually, but synchronously pruning keeps the admin UI clean and
// frees the hostname for the next grow (Tailscale auto-dedups by
// suffixing -1/-2/... when a hostname is already taken).
//
// apiTokenFile is the path to a tskey-api-* token. When the file doesn't
// exist or contains no recognisable token, this is a no-op that returns a
// soft error the caller can log without failing destroy.
func pruneTailnetDevices(ctx context.Context, apiTokenFile, hostnamePrefix string) (int, error) {
	token, err := readAPIToken(apiTokenFile)
	if err != nil {
		return 0, err
	}
	devices, err := listTailnetDevices(ctx, token)
	if err != nil {
		return 0, fmt.Errorf("list devices: %w", err)
	}
	var deleted int
	for _, d := range devices {
		if !strings.HasPrefix(d.Hostname, hostnamePrefix) {
			continue
		}
		if err := deleteTailnetDevice(ctx, token, d.ID); err != nil {
			return deleted, fmt.Errorf("delete device %s (%s): %w", d.ID, d.Hostname, err)
		}
		deleted++
	}
	return deleted, nil
}

func readAPIToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, tok := range strings.Fields(string(raw)) {
		if strings.HasPrefix(tok, "tskey-api-") {
			return tok, nil
		}
	}
	return "", fmt.Errorf("no tskey-api-* token found in %s", path)
}

type tailnetDevice struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

func listTailnetDevices(ctx context.Context, token string) ([]tailnetDevice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.tailscale.com/api/v2/tailnet/-/devices", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Devices []tailnetDevice `json:"devices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse devices: %w", err)
	}
	return out.Devices, nil
}

func deleteTailnetDevice(ctx context.Context, token, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://api.tailscale.com/api/v2/device/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}
