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
}

// SSMPathPrefix returns the canonical SSM Parameter Store prefix for a cluster's
// outputs: /bonsai/<name>/<env>/
func (c ClusterConfig) SSMPathPrefix() string {
	return "/bonsai/" + c.Name + "/" + c.Env + "/"
}
