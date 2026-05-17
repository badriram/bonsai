package config

// ClusterConfig is the input every provider receives. Same shape on every cloud.
type ClusterConfig struct {
	Provider string
	Name     string
	Env      string
	Region   string
	Workers  int
}

// SSMPathPrefix returns the canonical SSM Parameter Store prefix for a cluster's
// outputs: /bonsai/<name>/<env>/
func (c ClusterConfig) SSMPathPrefix() string {
	return "/bonsai/" + c.Name + "/" + c.Env + "/"
}
