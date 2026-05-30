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

	// TailnetKeySSMPath is the SSM parameter path holding a pre-auth key for
	// the tailnet (operator pre-creates it; Bonsai only reads). Example:
	//   /myorg/secrets/headscale-preauthkey
	TailnetKeySSMPath string
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
