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

	// Still to come (next change): cluster.Bootstrap installs CNPG + Valkey +
	// system-upgrade-controller + kured via the helm SDK, then writes
	// postgres_url + kv_url + namespace secrets.
	return provider.PlatformOutputs{
		ClusterEndpoint:    controlURL,
		KubeconfigLocation: "ssm://" + cfg.SSMPathPrefix() + "kubeconfig",
		PostgresURL:        "(pending in-cluster bootstrap)",
		KVURL:              "(pending in-cluster bootstrap)",
	}, nil
}

func (p *Provider) Destroy(ctx context.Context, name, env string) error {
	return fmt.Errorf("aws.Provider.Destroy: not yet implemented")
}

func (p *Provider) Status(ctx context.Context, name, env string) (provider.PlatformStatus, error) {
	return provider.PlatformStatus{}, fmt.Errorf("aws.Provider.Status: not yet implemented")
}

// accountID is a best-effort lookup used to name the shared backup bucket.
// Operator can override via env BONSAI_BACKUP_BUCKET in a later change.
func (p *Provider) accountID() string {
	if p.envLookup != nil {
		if v := p.envLookup("AWS_ACCOUNT_ID"); v != "" {
			return v
		}
	}
	return "unknown"
}
