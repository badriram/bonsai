package aws

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/badriram/bonsai/internal/cluster"
	bcfg "github.com/badriram/bonsai/internal/config"
	"github.com/badriram/bonsai/internal/provider"
	"github.com/badriram/bonsai/internal/secrets"
)

// Provider implements provider.PlatformProvider against AWS using the SDK
// directly — no CDK, no CloudFormation. State lives in resource tags and
// Parameter Store; reality is queried on every call.
type Provider struct {
	awsCfg    aws.Config
	ec2       *ec2.Client
	asg       *autoscaling.Client
	iam       *iam.Client
	ssm       *ssm.Client
	elbv2     *elasticloadbalancingv2.Client
	store     secrets.Store
	envLookup func(string) string
}

func New(ctx context.Context) (*Provider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	ssmClient := ssm.NewFromConfig(cfg)
	return &Provider{
		awsCfg:    cfg,
		ec2:       ec2.NewFromConfig(cfg),
		asg:       autoscaling.NewFromConfig(cfg),
		iam:       iam.NewFromConfig(cfg),
		ssm:       ssmClient,
		elbv2:     elasticloadbalancingv2.NewFromConfig(cfg),
		store:     secrets.NewParameterStore(ssmClient),
		envLookup: os.Getenv,
	}, nil
}

// Provision is idempotent: every step looks up by tag, creates if missing,
// updates if drifted. Safe to run from CI on every deploy.
func (p *Provider) Provision(ctx context.Context, cfg bcfg.ClusterConfig) (provider.PlatformOutputs, error) {
	if cfg.AdminCIDR != "" {
		_ = os.Setenv("BONSAI_ADMIN_CIDR", cfg.AdminCIDR)
	}
	net, err := p.ensureVPC(ctx, cfg.Name, cfg.Env, cfg.HAControl)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}

	backupBucket := "bonsai-backups-" + p.accountID()
	var extraSSMReads []string
	if cfg.TailnetMode() {
		extraSSMReads = append(extraSSMReads, cfg.TailnetKeySSMPath)
	}
	instanceProfile, err := p.ensureIAM(ctx, cfg.Name, cfg.Env, backupBucket, extraSSMReads...)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}

	if _, err := p.ensureK3sToken(ctx, cfg.Name, cfg.Env); err != nil {
		return provider.PlatformOutputs{}, err
	}

	var (
		controlURL string
		workerOpt  workerOpts
	)
	switch {
	case cfg.HAControl && cfg.TailnetMode():
		if err := p.ensureControlPlaneASG(ctx, controlPlaneHASpec{
			Name: cfg.Name, Env: cfg.Env,
			Net:               net,
			InstanceProfile:   instanceProfile,
			BackupBucket:      backupBucket,
			TailnetURL:        cfg.TailnetURL,
			TailnetKeySSMPath: cfg.TailnetKeySSMPath,
			TailnetTag:        cfg.TailnetTag,
		}); err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("control plane ASG (tailnet): %w", err)
		}
		// In tailnet mode the leader publishes its tailnet IP to SSM. The
		// kubeconfig stored in SSM already points at it; the human-friendly
		// CLUSTER_ENDPOINT we surface is best-effort here.
		controlURL = "tailnet://" + cfg.TailnetURL
		workerOpt = workerOpts{
			TailnetURL: cfg.TailnetURL, TailnetKeySSMPath: cfg.TailnetKeySSMPath,
			TailnetTag: cfg.TailnetTag,
		}
	case cfg.HAControl:
		nlb, err := p.ensureControlNLB(ctx, cfg.Name, cfg.Env, net)
		if err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("control NLB: %w", err)
		}
		if err := p.ensureControlPlaneASG(ctx, controlPlaneHASpec{
			Name: cfg.Name, Env: cfg.Env,
			Net:             net,
			InstanceProfile: instanceProfile,
			NLBDNS:          nlb.DNSName,
			TargetGroupArn:  nlb.TargetGroupArn,
			BackupBucket:    backupBucket,
		}); err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("control plane ASG: %w", err)
		}
		controlURL = "https://" + nlb.DNSName + ":6443"
		workerOpt = workerOpts{ControlPlaneURL: controlURL}
	default:
		eip, err := p.ensureControlEIP(ctx, cfg.Name, cfg.Env)
		if err != nil {
			return provider.PlatformOutputs{}, err
		}
		controlInstanceID, err := p.ensureControlPlane(ctx, controlPlaneSpec{
			Name: cfg.Name, Env: cfg.Env,
			Net:             net,
			InstanceProfile: instanceProfile,
			ControlIP:       eip.PublicIP,
			BackupBucket:    backupBucket,
		})
		if err != nil {
			return provider.PlatformOutputs{}, err
		}
		if err := p.associateControlEIP(ctx, eip, controlInstanceID); err != nil {
			return provider.PlatformOutputs{}, fmt.Errorf("associate control EIP: %w", err)
		}
		controlURL = "https://" + eip.PublicIP + ":6443"
		workerOpt = workerOpts{ControlPlaneURL: controlURL}
	}

	if err := p.waitForK3sReady(ctx, cfg.Name, cfg.Env); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("control plane never became ready: %w", err)
	}

	workerCount := int32(cfg.Workers)
	if workerCount < 1 {
		workerCount = 1
	}
	if err := p.ensureWorkers(ctx, cfg.Name, cfg.Env, workerCount, net, instanceProfile, workerOpt); err != nil {
		return provider.PlatformOutputs{}, err
	}

	kubeconfig, err := p.store.Read(ctx, cfg.SSMPathPrefix()+"kubeconfig")
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("fetch kubeconfig: %w", err)
	}

	out, err := cluster.Bootstrap(ctx, cluster.Config{
		Kubeconfig:   []byte(kubeconfig),
		Name:         cfg.Name,
		Env:          cfg.Env,
		BackupBucket: backupBucket,
		BackupRegion: p.awsCfg.Region,
	})
	if err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("in-cluster bootstrap: %w", err)
	}

	if err := p.store.Write(ctx, cfg.SSMPathPrefix()+"postgres_url", out.PostgresURL); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("write postgres_url: %w", err)
	}
	if err := p.store.Write(ctx, cfg.SSMPathPrefix()+"kv_url", out.KVURL); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("write kv_url: %w", err)
	}

	return provider.PlatformOutputs{
		ClusterEndpoint:    controlURL,
		KubeconfigLocation: "ssm://" + cfg.SSMPathPrefix() + "kubeconfig",
		PostgresURL:        out.PostgresURL,
		KVURL:              out.KVURL,
	}, nil
}

// accountID is a best-effort lookup used to name the shared backup bucket.
// Operator can override via env BONSAI_BACKUP_BUCKET in a later change.
// UpgradeK3s fetches the kubeconfig from Parameter Store and hands off to
// cluster.UpgradeK3s, which applies the SUC Plan CRDs.
func (p *Provider) UpgradeK3s(ctx context.Context, name, env, version string) error {
	kc, err := p.store.Read(ctx, "/bonsai/"+name+"/"+env+"/kubeconfig")
	if err != nil {
		return fmt.Errorf("fetch kubeconfig: %w", err)
	}
	return cluster.UpgradeK3s(ctx, []byte(kc), version)
}

// UpgradeComponent fetches the kubeconfig and delegates to
// cluster.UpgradeComponent, which re-runs the helm install (or manifest
// re-apply for SUC) against the pinned chart version.
func (p *Provider) UpgradeComponent(ctx context.Context, name, env, component string) error {
	kc, err := p.store.Read(ctx, "/bonsai/"+name+"/"+env+"/kubeconfig")
	if err != nil {
		return fmt.Errorf("fetch kubeconfig: %w", err)
	}
	return cluster.UpgradeComponent(ctx, cluster.Config{
		Kubeconfig: []byte(kc), Name: name, Env: env,
	}, component)
}

func (p *Provider) accountID() string {
	if p.envLookup != nil {
		if v := p.envLookup("AWS_ACCOUNT_ID"); v != "" {
			return v
		}
	}
	return "unknown"
}
