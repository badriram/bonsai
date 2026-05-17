package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	discocache "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

const fieldManager = "bonsai"

// applyManifestURL fetches a multi-document YAML over HTTP and server-side
// applies every object in it. Idempotent: re-applying the same manifest is a
// no-op via the fieldManager. The minimum viable kubectl-apply.
func applyManifestURL(ctx context.Context, restCfg *rest.Config, dyn dynamic.Interface, url string) error {
	body, err := fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}

	mapper, err := newRESTMapper(restCfg)
	if err != nil {
		return err
	}

	decoder := yaml.NewYAMLOrJSONDecoder(body, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode %s: %w", url, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := applyOne(ctx, dyn, mapper, obj); err != nil {
			return fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
}

func applyOne(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("map %s: %w", gvk, err)
	}
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return err
	}
	force := true
	opts := metav1.PatchOptions{FieldManager: fieldManager, Force: &force}

	res := dyn.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		_, err = res.Namespace(ns).Patch(ctx, obj.GetName(), types.ApplyPatchType, data, opts)
	} else {
		_, err = res.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, opts)
	}
	return err
}

func newRESTMapper(cfg *rest.Config) (meta.RESTMapper, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(discocache.NewMemCacheClient(dc)), nil
}

func fetch(ctx context.Context, url string) (io.Reader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(body), nil
}
