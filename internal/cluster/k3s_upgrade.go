package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const sucNamespace = "system-upgrade"

var planGVR = schema.GroupVersionResource{
	Group: "upgrade.cattle.io", Version: "v1", Resource: "plans",
}

// UpgradeK3s drives a k3s version bump by applying two SUC Plan CRDs:
//
//   server-plan: targets control-plane nodes, concurrency 1
//   agent-plan:  targets workers, prepares from server-plan so workers wait
//                until at least one control-plane node has upgraded
//
// SUC does the actual draining, binary swap (via rancher/k3s-upgrade), and
// uncordon. This function returns as soon as the Plans are applied —
// progress is observable via `kubectl get plans -n system-upgrade`.
func UpgradeK3s(ctx context.Context, kubeconfig []byte, version string) error {
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("parse kubeconfig: %w", err)
	}
	k8s, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	if err := ensureSUCNamespace(ctx, k8s); err != nil {
		return err
	}
	for _, plan := range []*unstructured.Unstructured{
		serverPlan(version),
		agentPlan(version),
	} {
		if err := applyPlan(ctx, dyn, plan); err != nil {
			return fmt.Errorf("apply %s: %w", plan.GetName(), err)
		}
	}
	return nil
}

func ensureSUCNamespace(ctx context.Context, k8s kubernetes.Interface) error {
	_, err := k8s.CoreV1().Namespaces().Get(ctx, sucNamespace, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}
	_, err = k8s.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: sucNamespace},
	}, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func serverPlan(version string) *unstructured.Unstructured {
	return plan("server-plan", version, map[string]any{
		"matchExpressions": []any{
			map[string]any{
				"key":      "node-role.kubernetes.io/control-plane",
				"operator": "Exists",
			},
		},
	}, nil)
}

func agentPlan(version string) *unstructured.Unstructured {
	return plan("agent-plan", version, map[string]any{
		"matchExpressions": []any{
			map[string]any{
				"key":      "node-role.kubernetes.io/control-plane",
				"operator": "DoesNotExist",
			},
		},
	}, map[string]any{
		// Workers wait for the server plan to make progress before they roll.
		"image": "rancher/k3s-upgrade",
		"args":  []any{"prepare", "server-plan"},
	})
}

func plan(name, version string, nodeSelector, prepare map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"concurrency":        1,
		"cordon":             true,
		"version":            version,
		"serviceAccountName": "system-upgrade",
		"nodeSelector":       nodeSelector,
		"upgrade":            map[string]any{"image": "rancher/k3s-upgrade"},
	}
	if prepare != nil {
		spec["prepare"] = prepare
	}
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("upgrade.cattle.io/v1")
	obj.SetKind("Plan")
	obj.SetName(name)
	obj.SetNamespace(sucNamespace)
	obj.Object["spec"] = spec
	return obj
}

func applyPlan(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) error {
	res := dyn.Resource(planGVR).Namespace(sucNamespace)
	existing, err := res.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err := res.Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}
