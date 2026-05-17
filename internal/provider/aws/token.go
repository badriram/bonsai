package aws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// ensureK3sToken returns the cluster join token, creating one on first call.
// The k3s server later overwrites it with its own node-token from user-data —
// the pre-seeded value is fine; both k3s and our worker.sh resolve it via SSM.
func (p *Provider) ensureK3sToken(ctx context.Context, name, env string) (string, error) {
	key := tokenSSMKey(name, env)
	got, err := p.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(key),
		WithDecryption: aws.Bool(true),
	})
	if err == nil {
		return aws.ToString(got.Parameter.Value), nil
	}
	var nf *ssmtypes.ParameterNotFound
	if !errors.As(err, &nf) {
		return "", fmt.Errorf("get token: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := p.store.Write(ctx, key, token); err != nil {
		return "", fmt.Errorf("put token: %w", err)
	}
	return token, nil
}

func tokenSSMKey(name, env string) string { return "/bonsai/" + name + "/" + env + "/token" }

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
