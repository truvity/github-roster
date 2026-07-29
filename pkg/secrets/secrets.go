// Package secrets reads mirrored credentials from AWS SSM Parameter Store.
//
// The service never talks to the password manager that is the source of
// truth. Credentials are mirrored into Parameter Store by a separate process
// and read from there, so this process needs one kind of credential (its own
// AWS identity) rather than two.
package secrets

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Reader reads parameters by exact name.
type Reader struct {
	client *ssm.Client
}

// NewReader returns a reader over the given client.
func NewReader(client *ssm.Client) *Reader { return &Reader{client: client} }

// Read returns one parameter's value, decrypting it when it is a SecureString.
//
// By exact name, never by path prefix. That is not fussiness: the mirror
// nests one source's prefix inside another's — `/secrets/google-workspace`
// and `/secrets/google-workspace/tp` — so a recursive read would hand one
// directory's credentials to the other.
func (r *Reader) Read(ctx context.Context, name string) (string, error) {
	out, err := r.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		// The name is safe to log; the value never appears in an error.
		return "", fmt.Errorf("read parameter %q: %w", name, err)
	}

	value := aws.ToString(out.Parameter.Value)
	if value == "" {
		return "", fmt.Errorf("parameter %q is empty", name)
	}

	return value, nil
}

// ReadAll reads several parameters under one prefix, by exact field name.
func (r *Reader) ReadAll(ctx context.Context, prefix string, fields ...string) (map[string]string, error) {
	values := make(map[string]string, len(fields))

	for _, field := range fields {
		value, err := r.Read(ctx, prefix+"/"+field)
		if err != nil {
			return nil, err
		}

		values[field] = value
	}

	return values, nil
}
