package aws

import (
	"context"
	"fmt"

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
// directly — no CDK, no CloudFormation. State lives in resource tags and SSM.
type Provider struct {
	awsCfg aws.Config
	ec2    *ec2.Client
	asg    *autoscaling.Client
	iam    *iam.Client
	ssm    *ssm.Client
	store  secrets.Store
}

func New(ctx context.Context) (*Provider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	ssmClient := ssm.NewFromConfig(cfg)
	return &Provider{
		awsCfg: cfg,
		ec2:    ec2.NewFromConfig(cfg),
		asg:    autoscaling.NewFromConfig(cfg),
		iam:    iam.NewFromConfig(cfg),
		ssm:    ssmClient,
		store:  secrets.NewParameterStore(ssmClient),
	}, nil
}

// Provision is idempotent: every step looks up by tag, creates if missing,
// updates if drifted. Safe to run from CI on every deploy.
func (p *Provider) Provision(ctx context.Context, cfg bcfg.ClusterConfig) (provider.PlatformOutputs, error) {
	// Phase 1 build order:
	// 1. ensureVPC          — VPC + public subnets + IGW + route table
	// 2. ensureIAM          — instance profile with SSM read for token + S3 for backups
	// 3. ensureK3sToken     — random token written to SSM
	// 4. ensureControlPlane — single EC2 with server user-data
	// 5. ensureWorkers      — ASG with worker user-data (joins via SSM token)
	// 6. cluster.Bootstrap  — install CNPG, Valkey, system-upgrade-controller, kured
	// 7. writeOutputs       — kubeconfig, postgres URL, KV URL to SSM
	_ = cluster.Bootstrap // wire in once the cluster is reachable
	return provider.PlatformOutputs{}, fmt.Errorf("aws.Provider.Provision: not yet implemented")
}

func (p *Provider) Destroy(ctx context.Context, name, env string) error {
	return fmt.Errorf("aws.Provider.Destroy: not yet implemented")
}

func (p *Provider) Status(ctx context.Context, name, env string) (provider.PlatformStatus, error) {
	return provider.PlatformStatus{}, fmt.Errorf("aws.Provider.Status: not yet implemented")
}
