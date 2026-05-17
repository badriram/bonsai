package secrets

import "context"

// Store abstracts where Bonsai writes cluster outputs. Consuming CIs read from
// the same path regardless of which provider provisioned the cluster.
type Store interface {
	Write(ctx context.Context, key, value string) error
	Read(ctx context.Context, key string) (string, error)
}
