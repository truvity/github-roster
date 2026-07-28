//go:build integration

package integration

import (
	"testing"
)

// TestBackendsReachable is the wiring check: it proves the run has the
// credentials and endpoints the later phases assume, and fails loudly on a
// half-configured environment instead of letting a typo hide behind skips.
func TestBackendsReachable(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		env := requireGitHub(t)
		id := runID(t)

		t.Logf("throwaway org %q, run id %q, console app %s, applier app %s",
			env.Org, id, env.ConsoleAppID, env.ApplierAppID)
	})

	t.Run("aws", func(t *testing.T) {
		endpoint := requireAWS(t)

		t.Logf("SSM/S3 endpoint %q", endpoint)
	})
}
