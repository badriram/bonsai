package secrets

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// ParameterStore writes to AWS Systems Manager Parameter Store, using
// SecureString parameters (KMS-encrypted at rest).
//
// We deliberately do NOT use AWS Secrets Manager:
//   - Secrets Manager: $0.40 per secret per month + API calls; built-in rotation
//     we don't need.
//   - Parameter Store: free for standard tier, $0.05 per 10k API calls; same
//     KMS encryption story for SecureString.
//
// Bonsai's outputs (kubeconfig, postgres URL, KV URL, k3s join token) are not
// rotation candidates — they're cluster identity. Parameter Store is the right
// fit and keeps the cost story honest.
type ParameterStore struct {
	client *ssm.Client
}

func NewParameterStore(c *ssm.Client) *ParameterStore { return &ParameterStore{client: c} }

func (p *ParameterStore) Write(ctx context.Context, key, value string) error {
	_, err := p.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(key),
		Value:     aws.String(value),
		Type:      types.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	return err
}

func (p *ParameterStore) Read(ctx context.Context, key string) (string, error) {
	out, err := p.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(key),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Parameter.Value), nil
}
