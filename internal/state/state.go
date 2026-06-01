// Package state persists what bonsai observed at provision time. Sits next
// to secrets in BONSAI_DATA_DIR/<name>-<env>/state.json. Operator does not
// edit this — bonsai writes it, reads it back for status and destroy.
//
// Three-tier split with config:
//   - bonsai.yaml (user-edited intent)
//   - state.json  (this — what bonsai observed)
//   - secrets/    (sensitive bytes, FileSecretStore)
//
// Writes are atomic (tmp file + rename) so a crash mid-write doesn't poison
// the next grow with a half-written state.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bcfg "github.com/badriram/bonsai/internal/config"
)

// SchemaVersion changes when the on-disk shape changes incompatibly. Old
// state files with a different SchemaVersion are treated as absent — bonsai
// re-discovers from cloud rather than trusting stale shape.
const SchemaVersion = "v1"

// State is what bonsai persisted for a cluster at last successful grow.
type State struct {
	SchemaVersion string    `json:"schema_version"`
	BonsaiVersion string    `json:"bonsai_version"`
	ProvisionedAt time.Time `json:"provisioned_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Declared is a snapshot of the resolved ClusterConfig at provision
	// time. Lets `bonsai plan` diff intent vs observed without re-parsing
	// the user's bonsai.yaml (which may have drifted since grow).
	Declared bcfg.ClusterConfig `json:"declared"`

	// Cluster endpoint as written into the kubeconfig (LB public IP, or
	// leader tailnet IP in tailnet mode).
	ClusterEndpoint string `json:"cluster_endpoint,omitempty"`

	// Per-cloud resource maps. Exactly one of these is populated per cluster.
	Hetzner *HetznerState `json:"hetzner,omitempty"`
	AWS     *AWSState     `json:"aws,omitempty"`
}

// HetznerState records what Bonsai created in Hetzner Cloud. IDs are the
// API's int64; names are kept for human-readable diffs.
type HetznerState struct {
	NetworkID    int64  `json:"network_id,omitempty"`
	FirewallID   int64  `json:"firewall_id,omitempty"`
	LBID         int64  `json:"lb_id,omitempty"`
	LBPublicIP   string `json:"lb_public_ip,omitempty"`
	LBPrivateIP  string `json:"lb_private_ip,omitempty"`
	SSHKeyID     int64  `json:"ssh_key_id,omitempty"`
	FloatingIPID int64  `json:"floating_ip_id,omitempty"`
	ImageName    string `json:"image_name,omitempty"`
	K3sVersion   string `json:"k3s_version,omitempty"`

	Servers []HetznerServer `json:"servers,omitempty"`

	// LeaderTailnetIP is the leader's tailnet address in tailnet mode.
	// Stable across cluster lifetime (tailnet IPs are sticky per host).
	LeaderTailnetIP string `json:"leader_tailnet_ip,omitempty"`
}

// HetznerServer is one server we created.
type HetznerServer struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"` // control-plane | worker
	Location   string `json:"location"`
	ServerType string `json:"server_type"`
	PublicIP   string `json:"public_ip,omitempty"`
	PrivateIP  string `json:"private_ip,omitempty"`
}

// AWSState is a placeholder for the AWS provider's state. Populated in a
// follow-up — AWS provisioning currently relies on resource tags for
// discovery and works without state.json.
type AWSState struct {
	// TODO: VPCID, ASGName, NLBArn, etc. when AWS provider wires this.
}

// Path returns the canonical on-disk location for a cluster's state file.
func Path(dataDir, name, env string) string {
	return filepath.Join(dataDir, name+"-"+env, "state.json")
}

// Read loads state.json. Returns (nil, nil) if the file doesn't exist —
// that's a normal first-grow case, not an error.
func Read(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Future-proof: treat a stale SchemaVersion as absent rather than
	// risking a mis-typed deserialization.
	if s.SchemaVersion != SchemaVersion {
		return nil, nil
	}
	return &s, nil
}

// Write persists state atomically (tmp + rename), creating parent dirs if
// needed. UpdatedAt is set to now; ProvisionedAt is preserved if the file
// already exists.
func Write(path string, s *State) error {
	if s.SchemaVersion == "" {
		s.SchemaVersion = SchemaVersion
	}
	now := time.Now().UTC()
	if s.ProvisionedAt.IsZero() {
		s.ProvisionedAt = now
	}
	s.UpdatedAt = now

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
