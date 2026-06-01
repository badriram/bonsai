package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// fileConfig is the on-disk schema of bonsai.yaml. Lives next to the app
// source (committable) or under BONSAI_DATA_DIR/<name>-<env>/config.yaml
// (auto-discovered). User-editable; bonsai's own state lives elsewhere.
type fileConfig struct {
	Name       string   `yaml:"name"`
	Env        string   `yaml:"env"`
	Provider   string   `yaml:"provider"`
	Region     string   `yaml:"region"`
	Workers    int      `yaml:"workers"`
	HAControl  bool     `yaml:"ha_control"`
	AdminCIDR  string   `yaml:"admin_cidr"`
	K3sVersion string   `yaml:"k3s_version"`
	Locations  []string `yaml:"locations"`

	ControlServerType string `yaml:"control_server_type"`
	WorkerServerType  string `yaml:"worker_server_type"`

	Postgres struct {
		Instances int `yaml:"instances"`
	} `yaml:"postgres"`

	Tailnet struct {
		Enabled     bool   `yaml:"enabled"`
		LoginServer string `yaml:"login_server"`
		Tag         string `yaml:"tag"`
		// Pick exactly one of the auth_key_* fields. Inline auth_key is
		// intentionally NOT supported — never commit a tskey-* to a config
		// file. Use auth_key_file (Hetzner) or auth_key_ssm (AWS).
		AuthKeyFile string `yaml:"auth_key_file"`
		AuthKeySSM  string `yaml:"auth_key_ssm"`
	} `yaml:"tailnet"`
}

// Load reads + validates bonsai.yaml at path. Returns a ClusterConfig ready
// to hand to a provider. Validation surfaces the credential and admin-CIDR
// shape errors that previously silently broke cloud-init at boot time.
func Load(path string) (ClusterConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ClusterConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var fc fileConfig
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil {
		return ClusterConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}

	cc := ClusterConfig{
		Provider:          fc.Provider,
		Name:              fc.Name,
		Env:               fc.Env,
		Region:            fc.Region,
		Workers:           fc.Workers,
		HAControl:         fc.HAControl,
		AdminCIDR:         fc.AdminCIDR,
		K3sVersion:        fc.K3sVersion,
		Locations:         fc.Locations,
		ControlServerType: fc.ControlServerType,
		WorkerServerType:  fc.WorkerServerType,
		PostgresInstances: fc.Postgres.Instances,
		TailnetTag:        fc.Tailnet.Tag,
	}
	if fc.Tailnet.Enabled {
		cc.TailnetURL = fc.Tailnet.LoginServer
		cc.TailnetKeyFile = fc.Tailnet.AuthKeyFile
		cc.TailnetKeySSMPath = fc.Tailnet.AuthKeySSM
	}

	if err := Validate(cc); err != nil {
		return cc, fmt.Errorf("validate %s: %w", path, err)
	}
	return cc, nil
}

// Validate enforces the invariants that earlier surfaced as silent boot-time
// breakage in real Hetzner smoke (bugs #11 + #12). Run after merging file +
// flag inputs, before the first cloud API call.
func Validate(cc ClusterConfig) error {
	if cc.Name == "" {
		return fmt.Errorf("name: required")
	}
	switch cc.Provider {
	case "aws", "hetzner", "libvirt":
	default:
		return fmt.Errorf("provider: must be aws | hetzner | libvirt (got %q)", cc.Provider)
	}

	// Without tailnet, the operator's source CIDR is the only thing that
	// keeps SSH + the k8s API reachable after the cluster firewall locks
	// down. Refusing to grow without it prevents post-firewall lockout
	// (bug #12 from the Hetzner tailnet smoke).
	if !cc.TailnetMode() && cc.AdminCIDR == "" {
		// Allow env-var fallback for legacy invocations.
		if os.Getenv("BONSAI_ADMIN_CIDR") == "" {
			return fmt.Errorf("admin_cidr: required unless tailnet.enabled (set in config or BONSAI_ADMIN_CIDR env var)")
		}
	}

	// Tailnet shape: if any tailnet field set, all the required ones must be.
	tailnetTouched := cc.TailnetURL != "" || cc.TailnetKeyFile != "" || cc.TailnetKeySSMPath != ""
	if tailnetTouched {
		if cc.TailnetURL == "" {
			return fmt.Errorf("tailnet.login_server: required when tailnet enabled")
		}
		if cc.TailnetKeyFile == "" && cc.TailnetKeySSMPath == "" {
			return fmt.Errorf("tailnet: one of auth_key_file (Hetzner) or auth_key_ssm (AWS) required")
		}
		if cc.TailnetKeyFile != "" && cc.TailnetKeySSMPath != "" {
			return fmt.Errorf("tailnet: auth_key_file and auth_key_ssm are mutually exclusive")
		}
		switch cc.Provider {
		case "hetzner", "libvirt":
			if cc.TailnetKeyFile == "" {
				return fmt.Errorf("tailnet on %s: auth_key_file required (no managed parameter store)", cc.Provider)
			}
		case "aws":
			if cc.TailnetKeySSMPath == "" {
				return fmt.Errorf("tailnet on aws: auth_key_ssm required (avoid putting secrets in a local file)")
			}
		}
	}

	// File-based credential: validate at load time so cloud-init never sees
	// a malformed cred. Tailscale's admin UI puts a two-line "Client ID:
	// ...\nClient secret: tskey-..." block on the clipboard — the literal
	// newline then broke YAML block-scalar indentation in user-data and
	// cloud-init silently dropped ssh_keys (bug #11).
	if cc.TailnetKeyFile != "" {
		raw, err := os.ReadFile(cc.TailnetKeyFile)
		if err != nil {
			return fmt.Errorf("tailnet.auth_key_file: %w", err)
		}
		found := false
		for _, tok := range strings.Fields(string(raw)) {
			if strings.HasPrefix(tok, "tskey-client-") || strings.HasPrefix(tok, "tskey-auth-") {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("tailnet.auth_key_file %s: no tskey-client-* or tskey-auth-* token found", cc.TailnetKeyFile)
		}
	}

	return nil
}
