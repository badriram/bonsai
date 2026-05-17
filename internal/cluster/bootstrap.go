package cluster

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config is what the AWS provider hands to Bootstrap after k3s is ready.
type Config struct {
	Kubeconfig   []byte
	Name         string
	Env          string
	BackupBucket string // S3 bucket for CNPG backups
	BackupRegion string
}

// Outputs are the connection strings Bonsai writes back to Parameter Store
// and mirrors into the app namespace as k8s Secrets.
type Outputs struct {
	PostgresURL string
	KVURL       string
}

// AppNamespace is where Postgres, Valkey, and the consuming app live.
// Pattern: <name>-<env>. Caller's Deployments target this namespace.
func (c Config) AppNamespace() string { return c.Name + "-" + c.Env }

// Bootstrap installs the Tier 1 in-cluster stack and returns the connection
// strings. Order matters:
//
//  1. cert-manager        — CNPG webhook dep
//  2. CloudNativePG       — operator
//  3. Postgres Cluster CR — actual database (2 replicas, S3 backups via IAM)
//  4. Valkey              — KV
//  5. kured               — safe rolling reboots
//  6. system-upgrade-controller — autonomous k3s version bumps
//
// Every step is idempotent: helm releases use upgrade-or-install; the CNPG CR
// is apply-style; namespaces and mirrored Secrets are create-or-update.
func Bootstrap(ctx context.Context, c Config) (Outputs, error) {
	clients, err := buildClients(c.Kubeconfig)
	if err != nil {
		return Outputs{}, err
	}
	defer clients.cleanup()

	if err := clients.helm.upgradeOrInstall(ctx, chartCertManager()); err != nil {
		return Outputs{}, fmt.Errorf("cert-manager: %w", err)
	}
	if err := clients.helm.upgradeOrInstall(ctx, chartCNPG()); err != nil {
		return Outputs{}, fmt.Errorf("cnpg operator: %w", err)
	}

	if err := ensureNamespace(ctx, clients.k8s, c.AppNamespace()); err != nil {
		return Outputs{}, err
	}

	pgURL, err := ensurePostgresCluster(ctx, clients.dyn, clients.k8s, c)
	if err != nil {
		return Outputs{}, fmt.Errorf("postgres cluster: %w", err)
	}

	if err := clients.helm.upgradeOrInstall(ctx, chartValkey(c.AppNamespace())); err != nil {
		return Outputs{}, fmt.Errorf("valkey: %w", err)
	}
	kvURL := fmt.Sprintf("redis://valkey-primary.%s.svc.cluster.local:6379", c.AppNamespace())

	if err := clients.helm.upgradeOrInstall(ctx, chartKured()); err != nil {
		return Outputs{}, fmt.Errorf("kured: %w", err)
	}
	if err := installSystemUpgradeController(ctx, clients.rest, clients.dyn); err != nil {
		return Outputs{}, fmt.Errorf("system-upgrade-controller: %w", err)
	}

	if err := mirrorSecret(ctx, clients.k8s, c.AppNamespace(), "bonsai-postgres", pgURL); err != nil {
		return Outputs{}, fmt.Errorf("mirror postgres secret: %w", err)
	}
	if err := mirrorSecret(ctx, clients.k8s, c.AppNamespace(), "bonsai-kv", kvURL); err != nil {
		return Outputs{}, fmt.Errorf("mirror kv secret: %w", err)
	}

	return Outputs{PostgresURL: pgURL, KVURL: kvURL}, nil
}

// UpgradeComponent re-runs the install for a single component against its
// pinned version. Use after bumping a chart version in charts.go.
//
// Components: cert-manager | cnpg | valkey | kured | system-upgrade-controller
func UpgradeComponent(ctx context.Context, c Config, component string) error {
	clients, err := buildClients(c.Kubeconfig)
	if err != nil {
		return err
	}
	defer clients.cleanup()

	switch component {
	case "cert-manager":
		return clients.helm.upgradeOrInstall(ctx, chartCertManager())
	case "cnpg":
		return clients.helm.upgradeOrInstall(ctx, chartCNPG())
	case "valkey":
		return clients.helm.upgradeOrInstall(ctx, chartValkey(c.AppNamespace()))
	case "kured":
		return clients.helm.upgradeOrInstall(ctx, chartKured())
	case "system-upgrade-controller":
		return installSystemUpgradeController(ctx, clients.rest, clients.dyn)
	default:
		return fmt.Errorf("unknown component %q (expected: cert-manager|cnpg|valkey|kured|system-upgrade-controller)", component)
	}
}

// clientBundle holds the typed + dynamic + helm clients all the cluster ops
// need, plus a cleanup for the temp kubeconfig file helm requires.
type clientBundle struct {
	rest    *rest.Config
	k8s     kubernetes.Interface
	dyn     dynamic.Interface
	helm    *helmClient
	cleanup func()
}

func buildClients(kubeconfig []byte) (*clientBundle, error) {
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	k8s, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	path, cleanup, err := writeTempKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return &clientBundle{
		rest: restCfg, k8s: k8s, dyn: dyn,
		helm:    newHelm(path),
		cleanup: cleanup,
	}, nil
}

func writeTempKubeconfig(data []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "bonsai-kubeconfig-*.yaml")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

func ensureNamespace(ctx context.Context, k8s kubernetes.Interface, name string) error {
	_, err := k8s.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}
	_, err = k8s.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func mirrorSecret(ctx context.Context, k8s kubernetes.Interface, ns, name, url string) error {
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"url": url},
	}
	existing, err := k8s.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err := k8s.CoreV1().Secrets(ns).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = k8s.CoreV1().Secrets(ns).Update(ctx, desired, metav1.UpdateOptions{})
	return err
}
