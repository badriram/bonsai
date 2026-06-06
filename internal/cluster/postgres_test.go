package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func existingWithSize(size string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"storage": map[string]any{"size": size}},
	}}
	return u
}

func TestReconcileStorageSize_NewClusterUsesDeclared(t *testing.T) {
	got, err := reconcileStorageSize(nil, "20Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "20Gi" {
		t.Fatalf("got %q, want 20Gi", got)
	}
}

func TestReconcileStorageSize_GrowAppliesDeclared(t *testing.T) {
	got, err := reconcileStorageSize(existingWithSize("10Gi"), "20Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "20Gi" {
		t.Fatalf("got %q, want 20Gi (CSI handles online expansion)", got)
	}
}

func TestReconcileStorageSize_ShrinkKeepsExisting(t *testing.T) {
	got, err := reconcileStorageSize(existingWithSize("20Gi"), "10Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "20Gi" {
		t.Fatalf("got %q, want 20Gi — never shrink", got)
	}
}

func TestReconcileStorageSize_EqualIsIdempotent(t *testing.T) {
	got, err := reconcileStorageSize(existingWithSize("50Gi"), "50Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "50Gi" {
		t.Fatalf("got %q, want 50Gi", got)
	}
}

func TestReconcileStorageSize_LexicalShrinkIsNotShrinkable(t *testing.T) {
	// "9Gi" > "10Gi" by string comparison but < by quantity; reconcile must
	// honor quantity semantics.
	got, err := reconcileStorageSize(existingWithSize("10Gi"), "9Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10Gi" {
		t.Fatalf("got %q, want 10Gi — 9Gi is a shrink and must be ignored", got)
	}
}

func TestReconcileStorageSize_MalformedDeclaredFails(t *testing.T) {
	if _, err := reconcileStorageSize(nil, "not-a-quantity"); err == nil {
		t.Fatal("expected error for malformed declared size")
	}
}

func TestReconcileStorageSize_MalformedExistingFallsBackToDeclared(t *testing.T) {
	got, err := reconcileStorageSize(existingWithSize("garbage"), "20Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "20Gi" {
		t.Fatalf("got %q, want 20Gi — unparseable existing should not block reconcile", got)
	}
}

func TestReconcileStorageSize_MissingStorageFieldUsesDeclared(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	got, err := reconcileStorageSize(u, "30Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "30Gi" {
		t.Fatalf("got %q, want 30Gi", got)
	}
}
