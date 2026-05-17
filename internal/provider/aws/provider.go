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
	_ = net // wired into control plane + ASG in the next change

	// Backup bucket name is deterministic per AWS account; created out-of-band
	// once (see docs/operator.md). The role grants access scoped to this cluster's prefix.
	backupBucket := "bonsai-backups-" + p.accountID(ctx)
	if _, err := p.ensureIAM(ctx, cfg.Name, cfg.Env, backupBucket); err != nil {
		return provider.PlatformOutputs{}, err
	}

	// Next changes wire in:
	//   ensureK3sToken  (random → Parameter Store)
	//   ensureControlPlane (EC2 with templated server.sh user-data)
	//   ensureWorkers   (ASG with worker.sh)
	//   cluster.Bootstrap (helm-install CNPG / Valkey / SUC / kured)
	//   writeOutputs    (kubeconfig + URLs into Parameter Store)
	_ = cluster.Bootstrap
	return provider.PlatformOutputs{}, fmt.Errorf("aws.Provider.Provision: VPC + IAM done; compute and bootstrap still TODO")
}

func (p *Provider) Destroy(ctx context.Context, name, env string) error {
	return fmt.Errorf("aws.Provider.Destroy: not yet implemented")
}

func (p *Provider) Status(ctx context.Context, name, env string) (provider.PlatformStatus, error) {
	return provider.PlatformStatus{}, fmt.Errorf("aws.Provider.Status: not yet implemented")
}

// accountID is a best-effort lookup used to name the shared backup bucket.
// Returns "unknown" on failure — only the caller's policy ARN is affected,
// and the operator can override via env BONSAI_BACKUP_BUCKET in a later change.
func (p *Provider) accountID(ctx context.Context) string {
	if p.envLookup != nil {
		if v := p.envLookup("AWS_ACCOUNT_ID"); v != "" {
			return v
		}
	}
	return "unknown"
}
