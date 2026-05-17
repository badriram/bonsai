package hetzner

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// RotateWorkers replaces every worker, one at a time:
//   1. Drain the k8s node (cordon + evict non-DaemonSet pods)
//   2. Delete the Hetzner server
//   3. Create a replacement with the target image + worker.sh user-data
//   4. Wait for the new node to register Ready in k8s
//
// Sequential, not parallel — keeps cluster capacity within (N-1) of desired
// at all times. imageRef is "latest" (use the most recent bonsai-node
// snapshot if one exists, else the base ubuntu-24.04 image) or a numeric
// Hetzner image ID.
func (p *Provider) RotateWorkers(ctx context.Context, name, env, imageRef string) error {
	workers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "worker")},
	})
	if err != nil {
		return err
	}
	if len(workers) == 0 {
		return fmt.Errorf("no workers found for %s/%s", name, env)
	}

	image, err := p.resolveWorkerImage(ctx, imageRef)
	if err != nil {
		return err
	}

	k8s, err := p.k8sClient(ctx, name, env)
	if err != nil {
		return err
	}

	// Need the token + control IP + ssh key for new worker cloud-init.
	token, err := p.store.Read(ctx, secretKey(name, env, tokenSecretKey))
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	endpoint, err := p.store.Read(ctx, secretKey(name, env, clusterEndpointKey))
	if err != nil {
		return fmt.Errorf("read endpoint: %w", err)
	}
	controlIP, err := extractIP(endpoint)
	if err != nil {
		return err
	}
	sshKey, err := p.ensureSSHKey(ctx, name, env)
	if err != nil {
		return err
	}
	userData, err := renderWorkerUserData(workerVars{
		ControlIP: controlIP, K3sVersion: defaultK3sVersion, Token: token,
	})
	if err != nil {
		return err
	}

	for i, w := range workers {
		nodeName := w.Name
		if err := drainNode(ctx, k8s, nodeName, 5*time.Minute); err != nil {
			return fmt.Errorf("drain %s: %w", nodeName, err)
		}
		if _, _, err := p.client.Server.DeleteWithResult(ctx, w); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete %s: %w", w.Name, err)
		}
		newName := fmt.Sprintf("bonsai-%s-%s-worker-%d", name, env, i+1)
		res, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
			Name:       newName,
			ServerType: &hcloud.ServerType{Name: defaultServerType},
			Image:      image,
			Location:   w.Datacenter.Location,
			SSHKeys:    []*hcloud.SSHKey{sshKey},
			UserData:   userData,
			Labels:     clusterLabels(name, env, "worker"),
		})
		if err != nil {
			return fmt.Errorf("create replacement %s: %w", newName, err)
		}
		_ = res
		if err := waitNodeReady(ctx, k8s, newName, 8*time.Minute); err != nil {
			return fmt.Errorf("new worker %s never Ready: %w", newName, err)
		}
	}
	return nil
}

func (p *Provider) resolveWorkerImage(ctx context.Context, ref string) (*hcloud.Image, error) {
	if ref == "" || ref == "latest" {
		baked, err := p.latestBakedImage(ctx)
		if err != nil {
			return nil, err
		}
		if baked != nil {
			return baked, nil
		}
		return &hcloud.Image{Name: defaultControlImage}, nil
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("--image must be 'latest' or a numeric Hetzner image ID, got %q", ref)
	}
	img, _, err := p.client.Image.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, fmt.Errorf("image %d not found", id)
	}
	return img, nil
}

func (p *Provider) k8sClient(ctx context.Context, name, env string) (kubernetes.Interface, error) {
	kc, err := p.store.Read(ctx, secretKey(name, env, kubeconfigSecretKey))
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(kc))
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(restCfg)
}

// extractIP pulls the host part out of an https://<ip>:6443 endpoint.
func extractIP(endpoint string) (string, error) {
	const prefix = "https://"
	if len(endpoint) <= len(prefix) || endpoint[:len(prefix)] != prefix {
		return "", fmt.Errorf("unexpected endpoint format: %s", endpoint)
	}
	rest := endpoint[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' {
			return rest[:i], nil
		}
	}
	return rest, nil
}
