package cluster

import (
	"context"
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Rancher doesn't publish a helm chart for system-upgrade-controller; the
// canonical install is `kubectl apply -f` against their release artifacts.
// We do the same via the dynamic client.
//
// SUC version is pinned here. Bumps should be intentional, not floating —
// the controller has cluster-wide privileges to drain nodes.
const sucVersion = "v0.14.2"

var sucManifestURLs = []string{
	"https://github.com/rancher/system-upgrade-controller/releases/download/" + sucVersion + "/crd.yaml",
	"https://github.com/rancher/system-upgrade-controller/releases/download/" + sucVersion + "/system-upgrade-controller.yaml",
}

// installSystemUpgradeController applies the SUC release manifests. Order
// matters: CRDs first so the controller can register watches against the
// Plan resource on first start.
func installSystemUpgradeController(ctx context.Context, restCfg *rest.Config, dyn dynamic.Interface) error {
	for _, u := range sucManifestURLs {
		if err := applyManifestURL(ctx, restCfg, dyn, u); err != nil {
			return fmt.Errorf("install SUC (%s): %w", u, err)
		}
	}
	return nil
}
