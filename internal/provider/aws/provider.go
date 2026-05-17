package aws

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/badri/bonsai/internal/cluster"
	bcfg "github.com/badri/bonsai/internal/config"
	"github.com/badri/bonsai/internal/provider"
	"github.com/badri/bonsai/internal/secrets"
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
		store:     secrets.NewParameterStore(ssmClient),
		envLookup: os.Getenv,
	}, nil
}

// Provision is idempotent: every step looks up by tag, creates if missing,
// updates if drifted. Safe to run from CI on every deploy.
func (p *Provider) Provision(ctx context.Context, cfg bcfg.ClusterConfig) (provider.PlatformOutputs, error) {
	net, err := p.ensureVPC(ctx, cfg.Name, cfg.Env)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}

	backupBucket := "bonsai-backups-" + p.accountID()
	instanceProfile, err := p.ensureIAM(ctx, cfg.Name, cfg.Env, backupBucket)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}

	if _, err := p.ensureK3sToken(ctx, cfg.Name, cfg.Env); err != nil {
		return provider.PlatformOutputs{}, err
	}

	controlDNS, err := p.ensureControlPlane(ctx, cfg.Name, cfg.Env, net, instanceProfile)
	if err != nil {
		return provider.PlatformOutputs{}, err
	}
	controlURL := "https://" + controlDNS + ":6443"

	if err := p.waitForK3sReady(ctx, cfg.Name, cfg.Env); err != nil {
		return provider.PlatformOutputs{}, fmt.Errorf("control plane never became ready: %w", err)
	}

	workerCount := int32(cfg.Workers)
	if workerCount < 1 {
		workerCount = 1
	}
	if err := p.ensureWorkers(ctx, cfg.Name, cfg.Env, workerCount, net, instanceProfile, controlURL); err != nil {
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
