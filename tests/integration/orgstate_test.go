//go:build integration

package integration

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/orgstate"
)

// newConsoleReader builds a reader using the console (read-only) App.
func newConsoleReader(t *testing.T, env githubEnv) *orgstate.Reader {
	t.Helper()

	reader, err := orgstate.NewReader(
		tokenSource(t, env.ConsoleAppID, env.ConsoleInstallationID, env.ConsolePrivateKey),
		env.Org, "")
	require.NoError(t, err)

	return reader
}

func tokenSource(t *testing.T, appID, installationID, privateKey string) *githubapp.TokenSource {
	t.Helper()

	app, err := strconv.ParseInt(appID, 10, 64)
	require.NoError(t, err, "app id must be numeric")

	installation, err := strconv.ParseInt(installationID, 10, 64)
	require.NoError(t, err, "installation id must be numeric")

	source, err := githubapp.NewTokenSource(githubapp.Credentials{
		AppID:          app,
		InstallationID: installation,
		PrivateKey:     []byte(privateKey),
	})
	require.NoError(t, err)

	return source
}

// TestConsoleAppCanRead exercises the read path the web tier actually uses,
// against a real organization.
//
// A mock would encode our beliefs about GitHub's membership model and then
// confirm them. The parts that are easy to get wrong — pagination, the
// admin/member split, invitations being absent from the members listing —
// only show up against the real API.
func TestConsoleAppCanRead(t *testing.T) {
	env := requireGitHub(t)
	ctx := context.Background()

	reader, err := orgstate.NewReader(
		tokenSource(t, env.ConsoleAppID, env.ConsoleInstallationID, env.ConsolePrivateKey),
		env.Org, "")
	require.NoError(t, err)

	state, err := reader.Read(ctx)
	require.NoError(t, err)

	require.Equal(t, env.Org, state.Org)
	require.NotEmpty(t, state.Members, "a throwaway org still has at least its owner")
	require.False(t, state.ReadAt.IsZero())

	var owners int

	for _, m := range state.Members {
		require.NotEmpty(t, m.Login)
		require.Contains(t, []orgstate.Role{orgstate.RoleAdmin, orgstate.RoleMember}, m.Role)

		if m.Role == orgstate.RoleAdmin {
			owners++
		}
	}

	// Owners are not this service's business to change, but they must be
	// read: a reconciler config that omitted them would propose removing
	// them.
	require.Positive(t, owners, "every organization has at least one owner")

	t.Logf("org %q: %d members (%d owners), %d invitations, %d teams",
		state.Org, len(state.Members), owners, len(state.Invitations), len(state.Teams))

	for _, team := range state.Teams {
		require.NotEmpty(t, team.Slug)
		require.Contains(t, state.TeamMembers, team.Slug, "every team's members must be read")
	}
}

// TestMembershipLookupIsCaseInsensitive guards the join against the most
// boring possible outage: GitHub logins are case-insensitive, and a mapping
// entry typed with different capitalization than the org reports would make
// a present member look absent — and a scheduled run would then remove them.
func TestMembershipLookupIsCaseInsensitive(t *testing.T) {
	env := requireGitHub(t)
	ctx := context.Background()

	reader, err := orgstate.NewReader(
		tokenSource(t, env.ConsoleAppID, env.ConsoleInstallationID, env.ConsolePrivateKey),
		env.Org, "")
	require.NoError(t, err)

	state, err := reader.Read(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, state.Members)

	login := state.Members[0].Login

	require.True(t, state.IsMember(login))
	require.True(t, state.IsMember(upper(login)), "membership lookup must ignore case")
	require.False(t, state.IsMember(login+"-definitely-not-a-member"))
}

func upper(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'a' && r <= 'z' {
			out[i] = r - 32
		}
	}

	return string(out)
}

// TestConsoleAppCannotWrite is the privilege boundary, tested rather than
// asserted in a document.
//
// The entire security argument of this service is that the web tier holds a
// credential which cannot mutate the organization. That claim is only worth
// something if something checks it — and it is exactly the sort of claim
// that quietly stops being true when somebody "fixes" a permissions list.
func TestConsoleAppCannotWrite(t *testing.T) {
	env := requireGitHub(t)
	ctx := context.Background()

	source := tokenSource(t, env.ConsoleAppID, env.ConsoleInstallationID, env.ConsolePrivateKey)

	token, err := source.Token(ctx)
	require.NoError(t, err, "the console App must at least authenticate")

	// Attempt the smallest real mutation: creating a team. If the console
	// credential can do this, the boundary does not exist.
	body := `{"name":"roster-boundary-probe-` + runID(t) + `"}`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.github.com/orgs/"+env.Org+"/teams", stringReader(body))
	require.NoError(t, err)

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Contains(t, []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound},
		resp.StatusCode,
		"the console App created a team — the read-only credential is not read-only")

	t.Logf("console App refused a team creation with %s, as it must", resp.Status)
}
