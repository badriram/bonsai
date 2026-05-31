package cluster

import (
	"context"
	"fmt"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
)

// helmClient is a thin wrapper around the helm SDK that gives us a single
// upgrade-or-install entry point per chart. No shelling out to a `helm`
// binary — we stay in-process for distribution reasons.
type helmClient struct {
	settings *cli.EnvSettings
}

type chartSpec struct {
	Release         string
	Namespace       string
	RepoURL         string
	Chart           string
	Version         string
	Values          map[string]any
	CreateNamespace bool
}

func newHelm(kubeconfigPath string) *helmClient {
	s := cli.New()
	s.KubeConfig = kubeconfigPath
	return &helmClient{settings: s}
}

func (h *helmClient) upgradeOrInstall(ctx context.Context, spec chartSpec) error {
	// helm's KubeClient picks up its default namespace from settings, NOT from
	// install.Namespace. Charts that don't set metadata.namespace explicitly
	// (e.g. cloudnative-pg) would otherwise land in the kubeconfig context's
	// default namespace instead of the release namespace.
	h.settings.SetNamespace(spec.Namespace)
	cfg := new(action.Configuration)
	noopLog := func(string, ...any) {}
	if err := cfg.Init(h.settings.RESTClientGetter(), spec.Namespace, "secret", noopLog); err != nil {
		return fmt.Errorf("helm init: %w", err)
	}

	hist := action.NewHistory(cfg)
	hist.Max = 1
	_, histErr := hist.Run(spec.Release)
	releaseExists := histErr == nil

	if !releaseExists {
		install := action.NewInstall(cfg)
		install.ReleaseName = spec.Release
		install.Namespace = spec.Namespace
		install.CreateNamespace = spec.CreateNamespace
		install.Wait = true
		install.Timeout = 8 * time.Minute
		install.ChartPathOptions.RepoURL = spec.RepoURL
		install.ChartPathOptions.Version = spec.Version

		chartPath, err := install.LocateChart(spec.Chart, h.settings)
		if err != nil {
			return fmt.Errorf("locate %s: %w", spec.Chart, err)
		}
		chrt, err := loader.Load(chartPath)
		if err != nil {
			return fmt.Errorf("load %s: %w", spec.Chart, err)
		}
		if _, err := install.RunWithContext(ctx, chrt, spec.Values); err != nil {
			return fmt.Errorf("install %s: %w", spec.Release, err)
		}
		return nil
	}

	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace = spec.Namespace
	upgrade.Wait = true
	upgrade.Timeout = 8 * time.Minute
	upgrade.ChartPathOptions.RepoURL = spec.RepoURL
	upgrade.ChartPathOptions.Version = spec.Version

	chartPath, err := upgrade.LocateChart(spec.Chart, h.settings)
	if err != nil {
		return fmt.Errorf("locate %s: %w", spec.Chart, err)
	}
	chrt, err := loader.Load(chartPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", spec.Chart, err)
	}
	if _, err := upgrade.RunWithContext(ctx, spec.Release, chrt, spec.Values); err != nil {
		return fmt.Errorf("upgrade %s: %w", spec.Release, err)
	}
	return nil
}
