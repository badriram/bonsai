package hetzner

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// drainNode cordons + evicts non-DaemonSet pods, then waits for the node to
// be empty. Minimal implementation — no PodDisruptionBudget grace, no force.
// Sufficient for Phase 2.1 rolling worker replacement.
func drainNode(ctx context.Context, k8s kubernetes.Interface, nodeName string, timeout time.Duration) error {
	node, err := k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !node.Spec.Unschedulable {
		node.Spec.Unschedulable = true
		if _, err := k8s.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("cordon: %w", err)
		}
	}

	pods, err := k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if skipDrain(pod) {
			continue
		}
		ev := &policyv1.Eviction{
			ObjectMeta:    metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
			DeleteOptions: &metav1.DeleteOptions{},
		}
		// Best-effort: PDB violations or already-deleted pods don't fail drain.
		_ = k8s.PolicyV1().Evictions(pod.Namespace).Evict(ctx, ev)
	}

	return waitNodeEmpty(ctx, k8s, nodeName, timeout)
}

func waitNodeEmpty(ctx context.Context, k8s kubernetes.Interface, nodeName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pods, err := k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return err
		}
		remaining := 0
		for i := range pods.Items {
			if !skipDrain(&pods.Items[i]) {
				remaining++
			}
		}
		if remaining == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("node %s did not drain within %s", nodeName, timeout)
}

func skipDrain(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
		return true
	}
	return false
}

// waitNodeReady polls for the new node to register and report Ready. Called
// after creating a replacement worker so RotateWorkers doesn't move to the
// next node before the cluster has caught up.
func waitNodeReady(ctx context.Context, k8s kubernetes.Interface, nodeName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		node, err := k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err == nil {
			for _, c := range node.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					return nil
				}
			}
		} else if !errors.IsNotFound(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("node %s never reported Ready within %s", nodeName, timeout)
}
