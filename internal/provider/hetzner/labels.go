package hetzner

// Hetzner uses Labels (key=value) where AWS uses Tags. Same role:
// idempotent lookup of "did we create this resource for this cluster?"
const (
	LabelCluster = "bonsai.cluster"
	LabelEnv     = "bonsai.env"
	LabelManaged = "bonsai.managed"
	LabelRole    = "bonsai.role" // control-plane | worker | control-fip | ssh-key
)

func clusterLabels(name, env, role string) map[string]string {
	return map[string]string{
		LabelCluster: name,
		LabelEnv:     env,
		LabelManaged: "true",
		LabelRole:    role,
	}
}

// roleSelector returns the Hetzner label-selector form for filtering by role.
func roleSelector(name, env, role string) string {
	return LabelCluster + "=" + name + "," +
		LabelEnv + "=" + env + "," +
		LabelManaged + "=true," +
		LabelRole + "=" + role
}

// clusterSelector matches every resource for the cluster regardless of role.
func clusterSelector(name, env string) string {
	return LabelCluster + "=" + name + "," +
		LabelEnv + "=" + env + "," +
		LabelManaged + "=true"
}
