package hetzner

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// BakeImage produces a fresh bonsai-node snapshot: creates an Ubuntu builder
// server with the build user-data, waits for it to shut down on its own (the
// builder script ends with `shutdown -h`), calls Server.CreateImage to
// snapshot the disk, tags the resulting image as bonsai-node:latest, removes
// the tag from the previous holder, deletes the builder, and prunes
// Bonsai-owned snapshots beyond the most recent 5.
const (
	bakeImageTagLatest  = "bonsai-node.latest"
	bakeImageTagVersion = "bonsai-node.k3s-version"
	bakeImageTagBuiltAt = "bonsai-node.built-at"
	bakeKeepN           = 5
	builderLocation     = defaultLocation
	builderServerType   = "cpx22"
	builderImage        = "ubuntu-24.04"
)

func (p *Provider) BakeImage(ctx context.Context, k3sVersion string) (string, error) {
	if k3sVersion == "" {
		k3sVersion = defaultK3sVersion
	}
	if id, err := p.findExistingBuilder(ctx); err != nil {
		return "", err
	} else if id != 0 {
		return "", fmt.Errorf("another bonsai-image-builder server is running (id=%d); wait or delete it first", id)
	}

	sshKey, err := p.ensureBuilderSSHKey(ctx)
	if err != nil {
		return "", fmt.Errorf("builder ssh key: %w", err)
	}

	userData, err := renderBuilderUserData(builderVars{K3sVersion: k3sVersion})
	if err != nil {
		return "", err
	}
	res, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       fmt.Sprintf("bonsai-image-builder-%d", time.Now().Unix()),
		ServerType: &hcloud.ServerType{Name: builderServerType},
		Image:      &hcloud.Image{Name: builderImage},
		Location:   &hcloud.Location{Name: builderLocation},
		SSHKeys:    []*hcloud.SSHKey{sshKey},
		UserData:   userData,
		Labels:     map[string]string{LabelManaged: "true", LabelRole: "image-builder"},
	})
	if err != nil {
		return "", fmt.Errorf("create builder: %w", err)
	}
	builder := res.Server
	defer func() {
		_, _, _ = p.client.Server.DeleteWithResult(ctx, builder)
	}()

	if err := p.waitForServerStatus(ctx, builder.ID, hcloud.ServerStatusOff, 25*time.Minute); err != nil {
		return "", fmt.Errorf("builder never powered off: %w", err)
	}

	imageRes, _, err := p.client.Server.CreateImage(ctx, builder, &hcloud.ServerCreateImageOpts{
		Type:        hcloud.ImageTypeSnapshot,
		Description: hcloud.Ptr("bonsai-node " + k3sVersion + " (" + time.Now().UTC().Format(time.RFC3339) + ")"),
		Labels: map[string]string{
			LabelManaged:        "true",
			bakeImageTagVersion: k3sVersion,
			bakeImageTagBuiltAt: time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return "", fmt.Errorf("create image: %w", err)
	}
	imageID := imageRes.Image.ID
	if err := p.waitForImageAvailable(ctx, imageID, 25*time.Minute); err != nil {
		return "", err
	}

	if err := p.promoteLatestImage(ctx, imageID); err != nil {
		return "", fmt.Errorf("promote latest: %w", err)
	}
	if err := p.pruneOldImages(ctx); err != nil {
		return strconv.FormatInt(imageID, 10), fmt.Errorf("bake succeeded (%d) but prune failed: %w", imageID, err)
	}
	return strconv.FormatInt(imageID, 10), nil
}

func (p *Provider) findExistingBuilder(ctx context.Context) (int64, error) {
	servers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: LabelRole + "=image-builder," + LabelManaged + "=true"},
	})
	if err != nil {
		return 0, err
	}
	if len(servers) > 0 {
		return servers[0].ID, nil
	}
	return 0, nil
}

// ensureBuilderSSHKey returns a Bonsai-owned SSH key for image baking,
// separate from per-cluster keys. The private half stays on the operator's
// machine but isn't actually used past Server.Create — the builder runs
// its user-data and shuts down on its own.
func (p *Provider) ensureBuilderSSHKey(ctx context.Context) (*hcloud.SSHKey, error) {
	keys, err := p.client.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: LabelRole + "=image-builder," + LabelManaged + "=true"},
	})
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		return keys[0], nil
	}
	pub, _, err := generateAuthorizedKey()
	if err != nil {
		return nil, err
	}
	key, _, err := p.client.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      "bonsai-image-builder",
		PublicKey: pub,
		Labels:    map[string]string{LabelManaged: "true", LabelRole: "image-builder"},
	})
	return key, err
}

func (p *Provider) waitForServerStatus(ctx context.Context, id int64, want hcloud.ServerStatus, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, _, err := p.client.Server.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if s != nil && s.Status == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("server %d did not reach %s within %s", id, want, timeout)
}

func (p *Provider) waitForImageAvailable(ctx context.Context, id int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		img, _, err := p.client.Image.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if img != nil && img.Status == hcloud.ImageStatusAvailable {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("image %d not available within %s", id, timeout)
}

// promoteLatestImage tags the new snapshot as bonsai-node:latest and removes
// that label from any previous holder.
func (p *Provider) promoteLatestImage(ctx context.Context, newID int64) error {
	prev, err := p.client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		Type:         []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status:       []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
		ListOpts:     hcloud.ListOpts{LabelSelector: bakeImageTagLatest + "=true"},
	})
	if err != nil {
		return err
	}
	for _, img := range prev {
		if img.ID == newID {
			continue
		}
		labels := img.Labels
		delete(labels, bakeImageTagLatest)
		if _, _, err := p.client.Image.Update(ctx, img, hcloud.ImageUpdateOpts{Labels: labels}); err != nil {
			return err
		}
	}
	target, _, err := p.client.Image.GetByID(ctx, newID)
	if err != nil {
		return err
	}
	labels := target.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels[bakeImageTagLatest] = "true"
	_, _, err = p.client.Image.Update(ctx, target, hcloud.ImageUpdateOpts{Labels: labels})
	return err
}

// pruneOldImages deletes Bonsai-owned snapshots beyond the most recent N.
// Hetzner bills snapshots monthly (~€0.0119/GB) — small numbers, but worth
// not accumulating.
func (p *Provider) pruneOldImages(ctx context.Context) error {
	imgs, err := p.client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		Type:     []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status:   []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
		ListOpts: hcloud.ListOpts{LabelSelector: LabelManaged + "=true"},
	})
	if err != nil {
		return err
	}
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].Created.After(imgs[j].Created) })
	if len(imgs) <= bakeKeepN {
		return nil
	}
	for _, img := range imgs[bakeKeepN:] {
		if _, err := p.client.Image.Delete(ctx, img); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// latestBakedImage returns the snapshot tagged bonsai-node:latest, or nil if
// none exists yet (caller falls back to the base ubuntu-24.04 image).
func (p *Provider) latestBakedImage(ctx context.Context) (*hcloud.Image, error) {
	imgs, err := p.client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		Type:     []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status:   []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
		ListOpts: hcloud.ListOpts{LabelSelector: bakeImageTagLatest + "=true"},
	})
	if err != nil {
		return nil, err
	}
	if len(imgs) == 0 {
		return nil, nil
	}
	return imgs[0], nil
}
