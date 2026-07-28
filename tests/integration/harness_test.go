//go:build integration

// Package integration holds the tests that need something real behind them:
// a throwaway GitHub organization, localstack for SSM and S3, a kind cluster
// for Jobs. See docs/development/testing.md.
//
// Every group skips itself when its backend is absent, so `just integration`
// on a fresh clone with nothing configured reports skips rather than
// failures.
package integration

import (
	"os"
	"testing"
)

// Environment contract. Kept in one place so a missing variable produces one
// clear skip message rather than a mystery failure three layers down.
const (
	envRunID = "ROSTER_TEST_RUN_ID"

	envOrg                   = "ROSTER_TEST_ORG"
	envConsoleAppID          = "ROSTER_TEST_CONSOLE_APP_ID"
	envConsoleInstallationID = "ROSTER_TEST_CONSOLE_INSTALLATION_ID"
	envConsolePrivateKey     = "ROSTER_TEST_CONSOLE_PRIVATE_KEY"
	envApplierAppID          = "ROSTER_TEST_APPLIER_APP_ID"
	envApplierInstallationID = "ROSTER_TEST_APPLIER_INSTALLATION_ID"
	envApplierPrivateKey     = "ROSTER_TEST_APPLIER_PRIVATE_KEY"

	envAWSEndpoint = "AWS_ENDPOINT_URL"
	envKubeconfig  = "ROSTER_TEST_KUBECONFIG"
)

// githubEnv is the credential set for the throwaway organization.
type githubEnv struct {
	Org string

	ConsoleAppID          string
	ConsoleInstallationID string
	ConsolePrivateKey     string

	ApplierAppID          string
	ApplierInstallationID string
	ApplierPrivateKey     string
}

// requireGitHub returns the GitHub credentials, or skips the test when the
// throwaway organization is not configured.
func requireGitHub(t *testing.T) githubEnv {
	t.Helper()

	env := githubEnv{
		Org:                   os.Getenv(envOrg),
		ConsoleAppID:          os.Getenv(envConsoleAppID),
		ConsoleInstallationID: os.Getenv(envConsoleInstallationID),
		ConsolePrivateKey:     os.Getenv(envConsolePrivateKey),
		ApplierAppID:          os.Getenv(envApplierAppID),
		ApplierInstallationID: os.Getenv(envApplierInstallationID),
		ApplierPrivateKey:     os.Getenv(envApplierPrivateKey),
	}

	if env.Org == "" {
		t.Skipf("%s is unset — see docs/development/testing.md", envOrg)
	}

	// Past this point the operator meant to run these, so a half-configured
	// environment is a failure rather than a skip: silently skipping here is
	// how a credential typo survives a green CI run.
	for name, value := range map[string]string{
		envConsoleAppID:          env.ConsoleAppID,
		envConsoleInstallationID: env.ConsoleInstallationID,
		envConsolePrivateKey:     env.ConsolePrivateKey,
		envApplierAppID:          env.ApplierAppID,
		envApplierInstallationID: env.ApplierInstallationID,
		envApplierPrivateKey:     env.ApplierPrivateKey,
	} {
		if value == "" {
			t.Fatalf("%s is set but %s is empty — the credential wiring is incomplete", envOrg, name)
		}
	}

	return env
}

// requireAWS returns the localstack endpoint, or skips.
func requireAWS(t *testing.T) string {
	t.Helper()

	endpoint := os.Getenv(envAWSEndpoint)
	if endpoint == "" {
		t.Skipf("%s is unset — start localstack, see docs/development/testing.md", envAWSEndpoint)
	}

	return endpoint
}

// runID namespaces everything a run creates in the shared throwaway
// organization, so parallel runs cannot collide and leftovers are
// attributable to the run that made them.
func runID(t *testing.T) string {
	t.Helper()

	id := os.Getenv(envRunID)
	if id == "" {
		t.Fatalf("%s is required: every object these tests create is tagged with it", envRunID)
	}

	return id
}
