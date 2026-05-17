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
//  5. system-upgrade-controller + kured — autonomous maintenance (TODO)
//
// Every step is idempotent: helm releases use upgrade-or-install; the CNPG CR
// is apply-style; namespaces and mirrored Secrets are create-or-update.
func Bootstrap(ctx context.Context, c Config) (Outputs, error) {
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(c.Kubeconfig)
	if err != nil {
		return Outputs{}, fmt.Errorf("parse kubeconfig: %w", err)
	}
	k8s, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return Outputs{}, fmt.Errorf("k8s client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return Outputs{}, fmt.Errorf("dynamic client: %w", err)
	}

	kubeconfigPath, cleanup, err := writeTempKubeconfig(c.Kubeconfig)
	if err != nil {
		return Outputs{}, err
	}
	defer cleanup()
	h := newHelm(kubeconfigPath)

	if err := h.upgradeOrInstall(ctx, chartSpec{
		Release: "cert-manager", Namespace: "cert-manager",
		RepoURL: "https://charts.jetstack.io", Chart: "cert-manager", Version: "v1.16.1",
		CreateNamespace: true,
		Values:          map[string]any{"crds": map[string]any{"enabled": true}},
	}); err != nil {
		return Outputs{}, fmt.Errorf("cert-manager: %w", err)
	}

	if err := h.upgradeOrInstall(ctx, chartSpec{
		Release: "cnpg", Namespace: "cnpg-system",
		RepoURL: "https://cloudnative-pg.github.io/charts", Chart: "cloudnative-pg", Version: "0.22.1",
		CreateNamespace: true,
	}); err != nil {
		return Outputs{}, fmt.Errorf("cnpg operator: %w", err)
	}

	if err := ensureNamespace(ctx, k8s, c.AppNamespace()); err != nil {
		return Outputs{}, err
	}

	pgURL, err := ensurePostgresCluster(ctx, dyn, k8s, c)
	if err != nil {
		return Outputs{}, fmt.Errorf("postgres cluster: %w", err)
	}

	if err := h.upgradeOrInstall(ctx, chartSpec{
		Release: "valkey", Namespace: c.AppNamespace(),
		RepoURL: "https://charts.bitnami.com/bitnami", Chart: "valkey", Version: "2.4.1",
		Values: map[string]any{
			"auth":         map[string]any{"enabled": false},
			"architecture": "standalone",
		},
	}); err != nil {
		return Outputs{}, fmt.Errorf("valkey: %w", err)
	}
	kvURL := fmt.Sprintf("redis://valkey-primary.%s.svc.cluster.local:6379", c.AppNamespace())

	// kured (Kubernetes Reboot Daemon) — drains and reboots nodes safely
	// after `apk upgrade` flips /var/run/reboot-required (or equivalent).
	// One node at a time, cluster-aware.
	if err := h.upgradeOrInstall(ctx, chartSpec{
		Release: "kured", Namespace: "kube-system",
		RepoURL: "https://kubereboot.github.io/charts", Chart: "kured", Version: "5.6.1",
	}); err != nil {
		return Outputs{}, fmt.Errorf("kured: %w", err)
	}

	// TODO: system-upgrade-controller — no official helm chart from Rancher,
	// so installation requires fetching and applying their manifest YAML via
	// the dynamic client. Will land in its own focused PR.

	if err := mirrorSecret(ctx, k8s, c.AppNamespace(), "bonsai-postgres", pgURL); err != nil {
		return Outputs{}, fmt.Errorf("mirror postgres secret: %w", err)
	}
	if err := mirrorSecret(ctx, k8s, c.AppNamespace(), "bonsai-kv", kvURL); err != nil {
		return Outputs{}, fmt.Errorf("mirror kv secret: %w", err)
	}

	return Outputs{PostgresURL: pgURL, KVURL: kvURL}, nil
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
