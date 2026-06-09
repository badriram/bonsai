// Package tailnet wraps the Tailscale management API for the bits Bonsai
// needs. Today: prune device records for clusters torn down via `bonsai
// destroy`, so the operator's admin UI doesn't accumulate stale entries
// and the next grow's hostname doesn't auto-dedup to <name>-1.
package tailnet

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

// DefaultBaseURL is Tailscale Cloud's management endpoint. Headscale and
// other self-hosted control planes don't (yet) speak the management API
// shape, so we only call out to api.tailscale.com.
const DefaultBaseURL = "https://api.tailscale.com"

// Client is a thin wrapper over the management API with a configurable
// base URL (for tests) and HTTP client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a Client pointed at Tailscale Cloud with a 15s timeout.
func New(token string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Device is the slim slice of the management API's device shape Bonsai
// reads. The full payload has dozens of fields; we only care about hostname
// matching and the ID needed to DELETE.
type Device struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

// ListDevices returns every device the API token's tailnet can see.
func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v2/tailnet/-/devices", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse devices: %w", err)
	}
	return out.Devices, nil
}

// DeleteDevice removes a single device by ID.
func (c *Client) DeleteDevice(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/v2/device/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
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

// PrunePrefix lists all devices and deletes every one whose hostname
// begins with prefix. Returns the count actually deleted. Stops on the
// first delete failure so the caller can surface why.
func (c *Client) PrunePrefix(ctx context.Context, prefix string) (int, error) {
	devices, err := c.ListDevices(ctx)
	if err != nil {
		return 0, fmt.Errorf("list devices: %w", err)
	}
	var deleted int
	for _, d := range devices {
		if !strings.HasPrefix(d.Hostname, prefix) {
			continue
		}
		if err := c.DeleteDevice(ctx, d.ID); err != nil {
			return deleted, fmt.Errorf("delete %s (%s): %w", d.ID, d.Hostname, err)
		}
		deleted++
	}
	return deleted, nil
}

// ReadAPITokenFile scans path for the first tskey-api-* whitespace-
// delimited token. Tolerates blank lines and a trailing newline so a
// quick `echo $TOKEN > file` works without ceremony.
func ReadAPITokenFile(path string) (string, error) {
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

// PruneFromFile is the convenience entry-point providers' Destroy paths
// call: read token file, list, delete by prefix. All errors are returned
// verbatim — best-effort vs hard-fail is the caller's choice.
func PruneFromFile(ctx context.Context, tokenFile, hostnamePrefix string) (int, error) {
	token, err := ReadAPITokenFile(tokenFile)
	if err != nil {
		return 0, err
	}
	return New(token).PrunePrefix(ctx, hostnamePrefix)
}
