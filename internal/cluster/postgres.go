package cluster

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// cnpgClusterGVR is the CNPG Cluster CRD this Bonsai release targets.
var cnpgClusterGVR = schema.GroupVersionResource{
	Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters",
}

const (
	postgresClusterName = "postgres"
	postgresInstances   = 2
	postgresVolumeSize  = "10Gi"
	postgresRetention   = "30d"
)

// ensurePostgresCluster applies a CNPG Cluster CR and waits for CNPG to create
// the <cluster>-app Secret. The Secret's `uri` field is what we return —
// CNPG-managed and rotated when credentials roll.
func ensurePostgresCluster(ctx context.Context, dyn dynamic.Interface, k8s kubernetes.Interface, c Config) (string, error) {
	ns := c.AppNamespace()
	if err := applyPostgresCluster(ctx, dyn, c, ns); err != nil {
		return "", err
	}
	return waitForPostgresAppSecret(ctx, k8s, ns)
}

func applyPostgresCluster(ctx context.Context, dyn dynamic.Interface, c Config, ns string) error {
	desired := &unstructured.Unstructured{}
	desired.SetAPIVersion("postgresql.cnpg.io/v1")
	desired.SetKind("Cluster")
	desired.SetName(postgresClusterName)
	desired.SetNamespace(ns)
	spec := map[string]any{
		"instances": postgresInstances,
		"storage":   map[string]any{"size": postgresVolumeSize},
	}
	// Backups only when a target bucket is configured. Providers without a
	// managed S3 equivalent (Hetzner Phase 2) leave this empty; configure
	// external S3 later if backups are required.
	if c.BackupBucket != "" {
		spec["backup"] = map[string]any{
			"barmanObjectStore": map[string]any{
				"destinationPath": fmt.Sprintf("s3://%s/%s/%s/postgres", c.BackupBucket, c.Name, c.Env),
				"s3Credentials":   map[string]any{"inheritFromIAMRole": true},
			},
			"retentionPolicy": postgresRetention,
		}
	}
	desired.Object["spec"] = spec

	res := dyn.Resource(cnpgClusterGVR).Namespace(ns)
	existing, err := res.Get(ctx, postgresClusterName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err := res.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	_, err = res.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

// waitForPostgresAppSecret polls for the Secret CNPG creates once the cluster
// is initialized. The Secret has a `uri` field with the full DSN.
func waitForPostgresAppSecret(ctx context.Context, k8s kubernetes.Interface, ns string) (string, error) {
	secretName := postgresClusterName + "-app"
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		sec, err := k8s.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			if uri, ok := sec.Data["uri"]; ok && len(uri) > 0 {
				return string(uri), nil
			}
		} else if !errors.IsNotFound(err) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return "", fmt.Errorf("CNPG never produced secret %s/%s within 15m", ns, secretName)
}
