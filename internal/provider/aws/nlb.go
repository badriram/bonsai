package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
)

// NLB infrastructure for the HA control plane.
//
//   - Network Load Balancer (internet-facing, multi-AZ) — stable DNS for
//     workers and operators
//   - Target Group on TCP 6443 — control plane EC2 instances register here
//   - Listener TCP:6443 → forward to TG
//
// The control plane ASG is wired to register/deregister itself with the TG
// in PR #19 (Phase 3 Part 2). For now ensureNLB just produces the NLB; the
// DNS name is returned for inclusion in --tls-san and worker user-data.

type nlbInfra struct {
	NLBArn        string
	TargetGroupArn string
	DNSName       string
}

func (p *Provider) ensureControlNLB(ctx context.Context, name, env string, net vpcInfra) (nlbInfra, error) {
	lb, err := p.findNLB(ctx, name, env)
	if err != nil {
		return nlbInfra{}, err
	}
	if lb == nil {
		lb, err = p.createNLB(ctx, name, env, net.SubnetIDs)
		if err != nil {
			return nlbInfra{}, fmt.Errorf("create NLB: %w", err)
		}
		if err := p.waitForNLBActive(ctx, aws.ToString(lb.LoadBalancerArn)); err != nil {
			return nlbInfra{}, err
		}
	}
	tg, err := p.ensureTargetGroup(ctx, name, env, net.VPCID)
	if err != nil {
		return nlbInfra{}, fmt.Errorf("target group: %w", err)
	}
	if err := p.ensureListener(ctx, aws.ToString(lb.LoadBalancerArn), aws.ToString(tg.TargetGroupArn)); err != nil {
		return nlbInfra{}, fmt.Errorf("listener: %w", err)
	}
	return nlbInfra{
		NLBArn:         aws.ToString(lb.LoadBalancerArn),
		TargetGroupArn: aws.ToString(tg.TargetGroupArn),
		DNSName:        aws.ToString(lb.DNSName),
	}, nil
}

func (p *Provider) findNLB(ctx context.Context, name, env string) (*elbtypes.LoadBalancer, error) {
	lbName := nlbName(name, env)
	out, err := p.elbv2.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{
		Names: []string{lbName},
	})
	if err != nil {
		if isELBNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out.LoadBalancers) == 0 {
		return nil, nil
	}
	return &out.LoadBalancers[0], nil
}

func (p *Provider) createNLB(ctx context.Context, name, env string, subnetIDs []string) (*elbtypes.LoadBalancer, error) {
	out, err := p.elbv2.CreateLoadBalancer(ctx, &elasticloadbalancingv2.CreateLoadBalancerInput{
		Name:    aws.String(nlbName(name, env)),
		Type:    elbtypes.LoadBalancerTypeEnumNetwork,
		Scheme:  elbtypes.LoadBalancerSchemeEnumInternetFacing,
		Subnets: subnetIDs,
		Tags: []elbtypes.Tag{
			{Key: aws.String(TagCluster), Value: aws.String(name)},
			{Key: aws.String(TagEnv), Value: aws.String(env)},
			{Key: aws.String(TagManaged), Value: aws.String("true")},
			{Key: aws.String(TagRole), Value: aws.String("control-nlb")},
		},
	})
	if err != nil {
		return nil, err
	}
	return &out.LoadBalancers[0], nil
}

func (p *Provider) ensureTargetGroup(ctx context.Context, name, env, vpcID string) (*elbtypes.TargetGroup, error) {
	tgName := nlbName(name, env) + "-tg"
	if len(tgName) > 32 {
		// ELB target group names are capped at 32 chars.
		tgName = tgName[:32]
	}
	existing, err := p.elbv2.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		Names: []string{tgName},
	})
	if err == nil && len(existing.TargetGroups) > 0 {
		tg := &existing.TargetGroups[0]
		if _, err := p.elbv2.ModifyTargetGroupAttributes(ctx, &elasticloadbalancingv2.ModifyTargetGroupAttributesInput{
			TargetGroupArn: tg.TargetGroupArn,
			Attributes: []elbtypes.TargetGroupAttribute{
				{Key: aws.String("preserve_client_ip.enabled"), Value: aws.String("false")},
			},
		}); err != nil {
			return nil, fmt.Errorf("set client IP preservation: %w", err)
		}
		return tg, nil
	}
	if err != nil && !isELBNotFound(err) {
		return nil, err
	}
	out, err := p.elbv2.CreateTargetGroup(ctx, &elasticloadbalancingv2.CreateTargetGroupInput{
		Name:                aws.String(tgName),
		Protocol:            elbtypes.ProtocolEnumTcp,
		Port:                aws.Int32(6443),
		VpcId:               aws.String(vpcID),
		TargetType:          elbtypes.TargetTypeEnumInstance,
		HealthCheckProtocol: elbtypes.ProtocolEnumTcp,
		HealthCheckPort:     aws.String("6443"),
		Tags: []elbtypes.Tag{
			{Key: aws.String(TagCluster), Value: aws.String(name)},
			{Key: aws.String(TagEnv), Value: aws.String(env)},
			{Key: aws.String(TagManaged), Value: aws.String("true")},
			{Key: aws.String(TagRole), Value: aws.String("control-tg")},
		},
	})
	if err != nil {
		return nil, err
	}
	tg := &out.TargetGroups[0]
	// In-VPC clients (workers + control plane joiners) hit this NLB DNS too.
	// With client IP preservation enabled (the default for instance targets),
	// the target's response goes direct to the source IP instead of back
	// through the NLB, so the TCP handshake never completes when source and
	// target are in the same VPC. Disable preservation so the NLB SNATs.
	if _, err := p.elbv2.ModifyTargetGroupAttributes(ctx, &elasticloadbalancingv2.ModifyTargetGroupAttributesInput{
		TargetGroupArn: tg.TargetGroupArn,
		Attributes: []elbtypes.TargetGroupAttribute{
			{Key: aws.String("preserve_client_ip.enabled"), Value: aws.String("false")},
		},
	}); err != nil {
		return nil, fmt.Errorf("disable client IP preservation: %w", err)
	}
	return tg, nil
}

func (p *Provider) ensureListener(ctx context.Context, nlbArn, tgArn string) error {
	existing, err := p.elbv2.DescribeListeners(ctx, &elasticloadbalancingv2.DescribeListenersInput{
		LoadBalancerArn: aws.String(nlbArn),
	})
	if err != nil {
		return err
	}
	for _, l := range existing.Listeners {
		if aws.ToInt32(l.Port) == 6443 {
			return nil
		}
	}
	_, err = p.elbv2.CreateListener(ctx, &elasticloadbalancingv2.CreateListenerInput{
		LoadBalancerArn: aws.String(nlbArn),
		Protocol:        elbtypes.ProtocolEnumTcp,
		Port:            aws.Int32(6443),
		DefaultActions: []elbtypes.Action{{
			Type:           elbtypes.ActionTypeEnumForward,
			TargetGroupArn: aws.String(tgArn),
		}},
	})
	return err
}

func (p *Provider) waitForNLBActive(ctx context.Context, arn string) error {
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := p.elbv2.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []string{arn},
		})
		if err != nil {
			return err
		}
		if len(out.LoadBalancers) > 0 && out.LoadBalancers[0].State != nil &&
			out.LoadBalancers[0].State.Code == elbtypes.LoadBalancerStateEnumActive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("NLB %s not active within 8m", arn)
}

func (p *Provider) destroyControlNLB(ctx context.Context, name, env string) error {
	lb, err := p.findNLB(ctx, name, env)
	if err != nil {
		return err
	}
	if lb != nil {
		listeners, err := p.elbv2.DescribeListeners(ctx, &elasticloadbalancingv2.DescribeListenersInput{
			LoadBalancerArn: lb.LoadBalancerArn,
		})
		if err == nil {
			for _, l := range listeners.Listeners {
				_, _ = p.elbv2.DeleteListener(ctx, &elasticloadbalancingv2.DeleteListenerInput{
					ListenerArn: l.ListenerArn,
				})
			}
		}
		if _, err := p.elbv2.DeleteLoadBalancer(ctx, &elasticloadbalancingv2.DeleteLoadBalancerInput{
			LoadBalancerArn: lb.LoadBalancerArn,
		}); err != nil && !isELBNotFound(err) {
			return fmt.Errorf("delete NLB: %w", err)
		}
	}
	// Target group can't be deleted while attached to a listener; the
	// listener delete above clears that. NLB deletion is async — give the TG
	// detach a moment before trying.
	time.Sleep(5 * time.Second)
	tgName := nlbName(name, env) + "-tg"
	if len(tgName) > 32 {
		tgName = tgName[:32]
	}
	tgs, err := p.elbv2.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{
		Names: []string{tgName},
	})
	if err != nil && !isELBNotFound(err) {
		return err
	}
	if err == nil {
		for _, tg := range tgs.TargetGroups {
			if _, err := p.elbv2.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
				TargetGroupArn: tg.TargetGroupArn,
			}); err != nil && !isELBNotFound(err) {
				return fmt.Errorf("delete TG: %w", err)
			}
		}
	}
	return nil
}

func nlbName(name, env string) string {
	n := "bonsai-" + name + "-" + env
	if len(n) > 32 {
		// ELB LB names are capped at 32 chars.
		n = n[:32]
	}
	return n
}

func isELBNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nfLB *elbtypes.LoadBalancerNotFoundException
	var nfTG *elbtypes.TargetGroupNotFoundException
	var nfL *elbtypes.ListenerNotFoundException
	return errors.As(err, &nfLB) || errors.As(err, &nfTG) || errors.As(err, &nfL)
}
