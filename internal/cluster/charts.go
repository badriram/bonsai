package cluster

// Pinned chart versions for every Tier 1 in-cluster component. Single source
// of truth: both Bootstrap and UpgradeComponent reference these. Bumps are
// intentional commits, never floating.

func chartCertManager() chartSpec {
	return chartSpec{
		Release: "cert-manager", Namespace: "cert-manager",
		RepoURL: "https://charts.jetstack.io", Chart: "cert-manager", Version: "v1.16.1",
		CreateNamespace: true,
		Values:          map[string]any{"crds": map[string]any{"enabled": true}},
	}
}

func chartCNPG() chartSpec {
	return chartSpec{
		Release: "cnpg", Namespace: "cnpg-system",
		RepoURL: "https://cloudnative-pg.github.io/charts", Chart: "cloudnative-pg", Version: "0.22.1",
		CreateNamespace: true,
	}
}

func chartValkey(appNamespace string) chartSpec {
	return chartSpec{
		Release: "valkey", Namespace: appNamespace,
		RepoURL: "https://charts.bitnami.com/bitnami", Chart: "valkey", Version: "2.4.1",
		Values: map[string]any{
			"auth":         map[string]any{"enabled": false},
			"architecture": "standalone",
		},
	}
}

func chartKured() chartSpec {
	return chartSpec{
		Release: "kured", Namespace: "kube-system",
		RepoURL: "https://kubereboot.github.io/charts", Chart: "kured", Version: "5.6.1",
	}
}
