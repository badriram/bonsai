package config

// ClusterConfig is the input every provider receives. Same shape on every cloud.
type ClusterConfig struct {
	Provider string
	Name     string
	Env      string
	Region   string
	Workers  int

	// HAControl, when true, provisions a 3-node embedded-etcd control plane
	// across multiple AZs behind a load balancer instead of a single EC2 with
	// an Elastic IP. Survives instance + AZ failure. Adds ~$60/month of
	// infra cost (NLB + 2 extra t3.small + multi-AZ data) — opt-in.
	HAControl bool

	// TailnetURL is the headscale/tailscale login server. When both this and
	// TailnetKeySSMPath are set, every node joins the operator's tailnet on
	// boot and the cluster API + worker join URL use a tailnet hostname
	// instead of public IPs. NLB and admin-CIDR machinery are skipped.
	TailnetURL string

	// TailnetKeySSMPath is the SSM parameter path holding the credential nodes
	// use to register with the tailnet. Two flavors supported:
	//   - OAuth client secret (recommended): tskey-client-... — Bonsai appends
	//     ?ephemeral=true&preauthorized=true so each node mints its own
	//     one-shot ephemeral key and is auto-pruned from the tailnet on death.
	//     Requires TailnetTag to be set and matching the OAuth client's tag
	//     scope + ACL.
	//   - Reusable pre-auth key: tskey-auth-... — used as-is. Key's baked-in
	//     tags/ephemerality apply. Operator owns rotation (max 90d lifetime).
	TailnetKeySSMPath string

	// TailnetTag is the device tag nodes advertise (e.g. "tag:bonsai"). Must
	// be defined in the operator's tailnet ACL. Required when using an OAuth
	// client secret; ignored for pre-auth keys (those have the tag baked in).
	TailnetTag string
}

// TailnetMode returns true when the cluster should join an operator-owned
// tailnet instead of exposing a public API endpoint.
func (c ClusterConfig) TailnetMode() bool {
	return c.TailnetURL != "" && c.TailnetKeySSMPath != ""
}

// SSMPathPrefix returns the canonical SSM Parameter Store prefix for a cluster's
// outputs: /bonsai/<name>/<env>/
func (c ClusterConfig) SSMPathPrefix() string {
	return "/bonsai/" + c.Name + "/" + c.Env + "/"
}
