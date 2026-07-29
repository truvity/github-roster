//go:build integration

package integration

import (
	"context"
	"testing"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/mapping/mappingtest"
)

// TestSSMStore runs the store contract against a real Parameter Store.
//
// The same assertions run against the in-memory store in the unit suite. If
// the two ever disagree, one of them is lying about what the service does —
// which is the entire reason the contract lives in its own package.
func TestSSMStore(t *testing.T) {
	requireAWS(t)

	client := newSSMClient(t)

	mappingtest.StoreSuite(t, func(t *testing.T) mapping.Store {
		// Each subtest gets its own prefix, so they neither see each other's
		// entries nor need cleaning up between assertions.
		prefix := "/" + runID(t) + "/" + t.Name() + "/"

		store := mapping.NewSSM(client, prefix)
		t.Cleanup(func() { emptyPrefix(t, client, store) })

		return store
	})
}

func newSSMClient(t *testing.T) *ssm.Client {
	t.Helper()

	// AWS_ENDPOINT_URL points the SDK at localstack; in production nothing
	// sets it and the SDK resolves the real endpoint.
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	require.NoError(t, err)

	return ssm.NewFromConfig(cfg)
}

// emptyPrefix deletes whatever a subtest left behind.
func emptyPrefix(t *testing.T, _ *ssm.Client, store mapping.Store) {
	t.Helper()

	ctx := context.Background()

	entries, err := store.List(ctx)
	if err != nil {
		t.Logf("cleanup: list failed: %v", err)

		return
	}

	for _, entry := range entries {
		if err := store.Delete(ctx, entry.Name); err != nil {
			t.Logf("cleanup: delete %q failed: %v", entry.Name, err)
		}
	}
}

// TestSSMVersionsAreTheHistory checks the one property the store gets for
// free from Parameter Store and the design leans on: editing an entry does
// not overwrite its past, it appends a version.
func TestSSMVersionsAreTheHistory(t *testing.T) {
	requireAWS(t)

	ctx := context.Background()
	client := newSSMClient(t)
	store := mapping.NewSSM(client, "/"+runID(t)+"/versions/")

	t.Cleanup(func() { emptyPrefix(t, client, store) })

	entry := mapping.Entry{Name: "A Person", GitHub: "octocat", Class: mapping.ClassEmployee}
	require.NoError(t, store.Put(ctx, entry))

	first, err := store.Get(ctx, entry.Name)
	require.NoError(t, err)
	require.NotEmpty(t, first.Revision)

	entry.K8s = "aperson"
	require.NoError(t, store.Put(ctx, entry))

	second, err := store.Get(ctx, entry.Name)
	require.NoError(t, err)

	require.NotEqual(t, first.Revision, second.Revision,
		"a write must advance the revision; SSM parameter versions are this store's audit history")
}
