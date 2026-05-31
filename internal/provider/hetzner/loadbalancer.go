package hetzner

import (
	"context"
	"fmt"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// HA control plane LB. Hetzner LB at type lb11 is ~€5.39/mo — much cheaper
// than AWS NLB (~$53/mo). Targets are the 3 control plane servers via their
// PRIVATE IPs in the cluster's Hetzner Network; targeting private IPs (rather
// than public) means Hetzner LB preserves source naturally and avoids the
// in-VPC asymmetric-routing gotcha we hit with AWS NLB preserve_client_ip.
//
// Service shape: TCP listen 6443 → TCP backend 6443. TCP health check on
// 6443. No PROXY protocol — k3s apiserver doesn't speak it natively and we
// don't need source-IP preservation for cluster bootstrap.
const (
	haLBType     = "lb11" // 1 vCPU, 0.5 GB, ~€5.39/mo
	haLBService  = 6443
	haLBHealthIv = 15 * time.Second
)

type lbInfra struct {
	LB        *hcloud.LoadBalancer
	PublicIP  string
	PrivateIP string // assigned within the cluster's Hetzner Network
}

func (p *Provider) ensureLoadBalancer(ctx context.Context, name, env string, network *hcloud.Network) (*lbInfra, error) {
	lbs, err := p.client.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "control-lb")},
	})
	if err != nil {
		return nil, err
	}
	var lb *hcloud.LoadBalancer
	if len(lbs) > 0 {
		lb = lbs[0]
	} else {
		res, _, err := p.client.LoadBalancer.Create(ctx, hcloud.LoadBalancerCreateOpts{
			Name:             "bonsai-" + name + "-" + env,
			LoadBalancerType: &hcloud.LoadBalancerType{Name: haLBType},
			NetworkZone:      haNetworkZoneEU,
			Network:          network,
			Labels:           clusterLabels(name, env, "control-lb"),
		})
		if err != nil {
			return nil, fmt.Errorf("create LB: %w", err)
		}
		lb = res.LoadBalancer
	}

	// Wait for the LB to be attached to the network so PrivateNet[].IP is set.
	if err := p.waitForLBNetwork(ctx, lb); err != nil {
		return nil, err
	}
	lb, _, err = p.client.LoadBalancer.GetByID(ctx, lb.ID)
	if err != nil {
		return nil, err
	}

	if err := p.ensureLBService(ctx, lb); err != nil {
		return nil, fmt.Errorf("LB service: %w", err)
	}

	info := &lbInfra{LB: lb, PublicIP: lb.PublicNet.IPv4.IP.String()}
	if len(lb.PrivateNet) > 0 {
		info.PrivateIP = lb.PrivateNet[0].IP.String()
	}
	return info, nil
}

func (p *Provider) ensureLBService(ctx context.Context, lb *hcloud.LoadBalancer) error {
	for _, s := range lb.Services {
		if s.ListenPort == haLBService {
			return nil
		}
	}
	_, _, err := p.client.LoadBalancer.AddService(ctx, lb, hcloud.LoadBalancerAddServiceOpts{
		Protocol:        hcloud.LoadBalancerServiceProtocolTCP,
		ListenPort:      hcloud.Ptr(haLBService),
		DestinationPort: hcloud.Ptr(haLBService),
		HealthCheck: &hcloud.LoadBalancerAddServiceOptsHealthCheck{
			Protocol: hcloud.LoadBalancerServiceProtocolTCP,
			Port:     hcloud.Ptr(haLBService),
			Interval: hcloud.Ptr(haLBHealthIv),
			Timeout:  hcloud.Ptr(10 * time.Second),
			Retries:  hcloud.Ptr(3),
		},
	})
	return err
}

// attachServerToLB registers a server as an LB target via its private IP
// inside the cluster network. UsePrivateIP avoids the public-internet
// hairpin and the source-preservation pitfall we hit on AWS NLB.
func (p *Provider) attachServerToLB(ctx context.Context, lb *hcloud.LoadBalancer, srv *hcloud.Server) error {
	for _, t := range lb.Targets {
		if t.Server != nil && t.Server.Server != nil && t.Server.Server.ID == srv.ID {
			return nil
		}
	}
	_, _, err := p.client.LoadBalancer.AddServerTarget(ctx, lb, hcloud.LoadBalancerAddServerTargetOpts{
		Server:       srv,
		UsePrivateIP: hcloud.Ptr(true),
	})
	return err
}

func (p *Provider) waitForLBNetwork(ctx context.Context, lb *hcloud.LoadBalancer) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		cur, _, err := p.client.LoadBalancer.GetByID(ctx, lb.ID)
		if err != nil {
			return err
		}
		if len(cur.PrivateNet) > 0 && cur.PrivateNet[0].IP != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("LB %d never got a private IP within 2m", lb.ID)
}

func (p *Provider) destroyLoadBalancer(ctx context.Context, name, env string) error {
	lbs, err := p.client.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: roleSelector(name, env, "control-lb")},
	})
	if err != nil {
		return err
	}
	for _, lb := range lbs {
		if _, err := p.client.LoadBalancer.Delete(ctx, lb); err != nil {
			return fmt.Errorf("delete LB %d: %w", lb.ID, err)
		}
	}
	return nil
}
