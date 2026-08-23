package configstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// ControlStore holds per-organization runtime control the operator flips
// from the UI — distinct from the reviewed, git-side config. Today: a pause
// switch. Read cheaply on every reconcile tick, so a pause takes effect at
// once without a restart or a config change.
type ControlStore interface {
	// Paused reports whether the org's reconcile loop is paused.
	Paused(ctx context.Context, org string) (bool, error)
	// SetPaused pauses or resumes the org's reconcile loop.
	SetPaused(ctx context.Context, org string, paused bool) error
}

// SSMControl stores control flags under <prefix>control/<org>/.
type SSMControl struct {
	client *ssm.Client
	prefix string
}

// NewSSMControl roots control flags at prefix (e.g. "/roster/").
func NewSSMControl(client *ssm.Client, prefix string) *SSMControl {
	return &SSMControl{client: client, prefix: prefix + "control/"}
}

func (s *SSMControl) pausedPath(org string) string {
	return s.prefix + org + "/paused"
}

// Paused reads the flag; an absent flag means not paused (the default).
func (s *SSMControl) Paused(ctx context.Context, org string) (bool, error) {
	out, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(s.pausedPath(org)),
	})
	if err != nil {
		var notFound *types.ParameterNotFound
		if errors.As(err, &notFound) {
			return false, nil
		}

		return false, fmt.Errorf("read pause flag for %q: %w", org, err)
	}

	return aws.ToString(out.Parameter.Value) == "true", nil
}

// SetPaused writes the flag.
func (s *SSMControl) SetPaused(ctx context.Context, org string, paused bool) error {
	value := "false"
	if paused {
		value = "true"
	}

	_, err := s.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(s.pausedPath(org)),
		Value:     aws.String(value),
		Type:      types.ParameterTypeString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("set pause flag for %q: %w", org, err)
	}

	return nil
}
