package hetzner

import (
	"context"
	"fmt"
	"net"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// HA clusters get a Hetzner Network so etcd peer / kubelet / flannel traffic
// stays private. Single-node mode still uses public IPs only — adding a
// network for the simple case is friction without payoff.
//
// IP layout (matches AWS posture):
//   10.0.0.0/16     network range
//   10.0.1.0/24     subnet in HA location[0] (default nbg1)
//   10.0.2.0/24     subnet in HA location[1] (default fsn1)
//   10.0.3.0/24     subnet in HA location[2] (default hel1)
//
// Hetzner network zones bracket which locations can share a network: eu-central
// covers nbg1/fsn1/hel1; us-east covers ash; us-west covers hil. We default to
// eu-central since that's the only zone with 3+ distinct locations today.
const (
	haNetworkRange  = "10.0.0.0/16"
	haNetworkZoneEU = "eu-central"
)

// haLocations returns the default HA spread when the operator hasn't supplied
// an explicit list. nbg1+fsn1+hel1 are all in eu-central so they can share one
// Hetzner Network.
func haLocations() []string { return []string{"nbg1", "fsn1", "hel1"} }

// haSubnetForIndex returns the /24 CIDR for the i-th HA subnet (0-indexed).
func haSubnetForIndex(i int) string {
	return fmt.Sprintf("10.0.%d.0/24", i+1)
}

// ensureNetwork is idempotent: returns the existing Hetzner Network for the
// cluster if present, otherwise creates one with a subnet per HA location.
func (p *Provider) ensureNetwork(ctx context.Context, name, env string, locations []string) (*hcloud.Network, error) {
	nets, err := p.client.Network.AllWithOpts(ctx, hcloud.NetworkListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "network")},
	})
	if err != nil {
		return nil, err
	}
	if len(nets) > 0 {
		return nets[0], nil
	}

	_, ipNet, err := net.ParseCIDR(haNetworkRange)
	if err != nil {
		return nil, err
	}

	subnets := make([]hcloud.NetworkSubnet, 0, len(locations))
	for i, loc := range locations {
		_, sub, err := net.ParseCIDR(haSubnetForIndex(i))
		if err != nil {
			return nil, err
		}
		_ = loc // location is implicit via the network zone for cloud-type subnets
		subnets = append(subnets, hcloud.NetworkSubnet{
			Type:        hcloud.NetworkSubnetTypeCloud,
			IPRange:     sub,
			NetworkZone: haNetworkZoneEU,
		})
	}

	res, _, err := p.client.Network.Create(ctx, hcloud.NetworkCreateOpts{
		Name:    "bonsai-" + name + "-" + env,
		IPRange: ipNet,
		Subnets: subnets,
		Labels:  clusterLabels(name, env, "network"),
	})
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}
	return res, nil
}

// destroyNetwork is called from Destroy. Idempotent: skips if no network is
// found. Servers must already be detached (Hetzner refuses Delete otherwise).
func (p *Provider) destroyNetwork(ctx context.Context, name, env string) error {
	nets, err := p.client.Network.AllWithOpts(ctx, hcloud.NetworkListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "network")},
	})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if _, err := p.client.Network.Delete(ctx, n); err != nil {
			return fmt.Errorf("delete network %d: %w", n.ID, err)
		}
	}
	return nil
}
