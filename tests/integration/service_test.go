//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/gofiber/fiber/v3"
	"github.com/neilotoole/slogt/v2"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/app"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/roster"
	"github.com/truvity/github-roster/pkg/server"
	"github.com/truvity/github-roster/pkg/version"
)

// TestServiceStartsAndServesTheRoster is the end-to-end test: the service's
// REAL startup path, from a configuration document to a served roster.
//
// It matters because every other test drives a package directly. This one
// goes through the path the binary uses — read the App credentials from
// Parameter Store, authenticate as the App, read the organization, join —
// so a mistake in the wiring (a misspelled parameter field, a credential
// read that never happens, deps left nil) is caught here and nowhere else.
//
// The parameter store starts EMPTY, exactly as it does in CI. The test
// seeds it first, from the same environment the workflow provides, which is
// what makes the local run and the CI run the same run.
func TestServiceStartsAndServesTheRoster(t *testing.T) {
	env := requireGitHub(t)
	requireAWS(t)

	ctx := context.Background()
	client := newSSMClient(t)

	// Namespaced per run so concurrent CI runs cannot see each other's
	// credentials or mapping.
	prefix := "/" + runID(t) + "/service"
	consolePrefix := prefix + "/secrets/console"
	mappingPrefix := prefix + "/roster/"

	seedCredentials(t, client, consolePrefix, env.ConsoleAppID, env.ConsoleInstallationID, env.ConsolePrivateKey)

	// A mapping entry for somebody who is genuinely in the sandbox org, so
	// the join has a real member to match rather than only warnings.
	store := mapping.NewSSM(client, mappingPrefix)
	t.Cleanup(func() { emptyPrefix(t, client, store) })

	member := firstMemberLogin(t, env)

	require.NoError(t, store.Put(ctx, mapping.Entry{
		Name:   "Test Person",
		GitHub: member,
		K8s:    "tperson",
		Class:  mapping.ClassEmployee,
	}))

	cfg, err := config.Parse(fmt.Appendf(nil, `
oidc: {disabled: true}
mapping: {ssmPrefix: %q}
audit: {bucket: roster-integration}
orgs:
  - name: %q
    consoleAppSSM: %q
    applierAppSSM: %q
    teams:
      robots: {pinned: true}
`, mappingPrefix, env.Org, consolePrefix, consolePrefix+"-applier"))
	require.NoError(t, err)

	// The real wiring, as the binary builds it.
	deps, err := app.BuildDeps(ctx, slogt.New(t), cfg, version.Info{Version: "integration"})
	require.NoError(t, err, "the service must start from configuration alone")

	fiberApp := server.NewApp(deps)

	t.Run("serves the joined roster", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/roster", http.NoBody)

		resp, err := fiberApp.Test(req, fiber.TestConfig{Timeout: testTimeout})
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

		var joined roster.Roster
		require.NoError(t, json.Unmarshal(body, &joined))

		require.False(t, joined.GeneratedAt.IsZero())
		require.Len(t, joined.People, 1)

		person := joined.People[0]
		require.Equal(t, "Test Person", person.Name)
		require.Equal(t, member, person.GitHub)

		// The org was read through the App credentials that this test put
		// into Parameter Store and the service read back out — which is
		// the whole point of the exercise.
		m := person.Orgs[env.Org]
		require.True(t, m.Member, "the mapped person should be seen as a member of %s", env.Org)
	})

	t.Run("health needs no credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)

		resp, err := server.NewHealthApp(deps).Test(req, fiber.TestConfig{Timeout: testTimeout})
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// TestServiceRefusesToStartWithoutCredentials pins the fail-fast promise:
// a missing parameter must stop the rollout, not surface as a broken page
// an hour later when somebody opens it.
func TestServiceRefusesToStartWithoutCredentials(t *testing.T) {
	requireAWS(t)
	requireGitHub(t)

	cfg, err := config.Parse(fmt.Appendf(nil, `
oidc: {disabled: true}
mapping: {ssmPrefix: %q}
audit: {bucket: roster-integration}
orgs:
  - name: example-org
    consoleAppSSM: /%s/definitely/not/here
    applierAppSSM: /%s/definitely/not/here-applier
`, "/"+runID(t)+"/absent/", runID(t), runID(t)))
	require.NoError(t, err)

	_, err = app.BuildDeps(context.Background(), slogt.New(t), cfg, version.Info{Version: "integration"})

	require.Error(t, err, "startup must fail when credentials are absent")
	require.ErrorContains(t, err, "github-app-id",
		"and the error must name the parameter that is missing")
}

// testTimeout bounds one in-process request. Generous because a real
// GitHub read happens inside it — and typed, because fiber.TestConfig
// takes a time.Duration: an untyped 60_000 there means 60 MICROseconds and
// fails with a bare "got empty response".
const testTimeout = 60 * time.Second

// seedCredentials writes an App's credentials where the service expects
// them. In CI the parameter store is a fresh, empty localstack, so the test
// must put them there itself — the same values the workflow hands it as
// environment variables.
func seedCredentials(t *testing.T, client *ssm.Client, prefix, appID, installationID, privateKey string) {
	t.Helper()

	ctx := context.Background()

	for field, value := range map[string]string{
		"github-app-id":          appID,
		"github-installation-id": installationID,
		"github-private-key":     privateKey,
	} {
		name := prefix + "/" + field

		_, err := client.PutParameter(ctx, &ssm.PutParameterInput{
			Name:      aws.String(name),
			Value:     aws.String(value),
			Type:      types.ParameterTypeSecureString,
			Overwrite: aws.Bool(true),
		})
		require.NoError(t, err)

		t.Cleanup(func() {
			_, _ = client.DeleteParameter(context.Background(), &ssm.DeleteParameterInput{Name: aws.String(name)})
		})
	}
}

// firstMemberLogin returns a login that really is a member of the sandbox
// organization, so the join has something to match.
func firstMemberLogin(t *testing.T, env githubEnv) string {
	t.Helper()

	reader := newConsoleReader(t, env)

	members, err := reader.Members(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, members, "the throwaway org should have at least its owner")

	return members[0].Login
}
