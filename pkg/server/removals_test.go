package server_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/neilotoole/slogt/v2"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/applier"
	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/server"
	"github.com/truvity/github-roster/pkg/ui"
	"github.com/truvity/github-roster/pkg/version"
)

// fakeOrgReader serves a fixed state.
type fakeOrgReader struct {
	state *orgstate.State
	err   error
}

func (f *fakeOrgReader) Read(context.Context) (*orgstate.State, error) { return f.state, f.err }

// fakeRunner records requests and answers success. It can also block, to
// prove the serialization.
type fakeRunner struct {
	requests []applier.Request
	err      error
	started  chan struct{}
	release  chan struct{}
}

func (f *fakeRunner) Run(_ context.Context, req applier.Request) (*applier.Run, error) {
	f.requests = append(f.requests, req)

	if f.started != nil {
		f.started <- struct{}{}
		<-f.release
	}

	if f.err != nil {
		return &applier.Run{JobName: "failed-job"}, f.err
	}

	return &applier.Run{
		JobName:   "roster-removals-" + req.RunID,
		Confirmed: req.Confirm,
		Succeeded: true,
		StartedAt: time.Now(),
	}, nil
}

// fakeDirSource feeds the directory cache a fixed snapshot.
type fakeDirSource struct{ snap *directory.Snapshot }

func (f *fakeDirSource) Name() string { return f.snap.Source }

func (f *fakeDirSource) Fetch(context.Context) (*directory.Snapshot, error) { return f.snap, nil }

// sweepWorld builds a Deps with two mapped people — one live, one gone —
// where the gone one is still a member.
func sweepWorld(t *testing.T, runner server.JobRunner) (*server.Deps, *audit.Memory, *fakeDirSource) {
	t.Helper()

	cfg, err := config.Parse([]byte(`
oidc: {disabled: true}
sources:
  - name: corp
    ssmPrefix: /secrets/directory/corp
    domains: [example.com]
orgs:
  - name: example-org
    consoleAppSSM: /secrets/roster/console/example-org
    applierAppSSM: /secrets/roster/applier/example-org
audit: {bucket: b}
`))
	require.NoError(t, err)

	store := mapping.NewMemory()
	ctx := context.Background()
	require.NoError(t, store.Put(ctx, mapping.Entry{Name: "Ada Lovelace", GitHub: "ada", Class: mapping.ClassEmployee}))
	require.NoError(t, store.Put(ctx, mapping.Entry{Name: "Gone Person", GitHub: "gone", Class: mapping.ClassEmployee}))

	source := &fakeDirSource{snap: &directory.Snapshot{
		Source: "corp",
		Users: []directory.User{
			{Name: "Ada Lovelace", Email: "ada@example.com", Live: true},
			{Name: "Gone Person", Email: "gone@example.com", Live: false},
		},
		Groups:    map[string][]string{},
		FetchedAt: time.Now(),
	}}

	renderer, err := ui.NewRenderer("test")
	require.NoError(t, err)

	sink := audit.NewMemory()

	deps := &server.Deps{
		Logger:      slogt.New(t),
		Config:      cfg,
		Auth:        &fixedAuth{role: auth.RoleOperator},
		Renderer:    renderer,
		Version:     version.Info{Version: "test"},
		Mapping:     store,
		Directories: directory.NewSet(slogt.New(t), source),
		Orgs: map[string]server.OrgReader{
			"example-org": &fakeOrgReader{state: &orgstate.State{
				Org: "example-org",
				Members: []orgstate.Member{
					{Login: "boss", Role: orgstate.RoleAdmin},
					{Login: "ada", Role: orgstate.RoleMember},
					{Login: "gone", Role: orgstate.RoleMember},
					{Login: "stranger", Role: orgstate.RoleMember},
				},
				TeamMembers: map[string][]string{},
			}},
		},
		Applier:     runner,
		Audit:       sink,
		ApplierApps: map[string]server.ApplierApp{"example-org": {AppID: "1", InstallationID: "2"}},
	}

	return deps, sink, source
}

// The sweep's whole contract in one test: removals-only, confirmed without
// a human, removing exactly the positively-gone person and nobody else.
func TestSweepRemovesOnlyThePositivelyGone(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	deps, sink, _ := sweepWorld(t, runner)

	summary, err := deps.RunRemovals(context.Background())
	require.NoError(t, err)
	require.Len(t, summary.Orgs, 1)

	outcome := summary.Orgs[0]
	require.Empty(t, outcome.Error)
	require.Equal(t, []string{"gone"}, outcome.Removing,
		"the live person and the unidentified stranger must both stay")

	require.Len(t, runner.requests, 1)
	req := runner.requests[0]
	require.Equal(t, peribolos.ModeRemovalsOnly, req.Result.Mode)
	require.True(t, req.Confirm, "the unattended sweep applies; that is its purpose")
	require.Empty(t, req.Result.Adding)
	require.Equal(t, "schedule", req.Actor)

	records, err := sink.List(context.Background(), "example-org", 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, audit.TriggerSchedule, records[0].Trigger)
	require.Equal(t, []string{"gone"}, records[0].Removing)
}

// Nothing to do is the healthy case: no Job, and no audit heartbeat.
func TestSweepSkipsWhenNothingToRemove(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	deps, sink, _ := sweepWorld(t, runner)

	// Everyone is live now.
	deps.Orgs["example-org"] = &fakeOrgReader{state: &orgstate.State{
		Org: "example-org",
		Members: []orgstate.Member{
			{Login: "ada", Role: orgstate.RoleMember},
		},
		TeamMembers: map[string][]string{},
	}}

	summary, err := deps.RunRemovals(context.Background())
	require.NoError(t, err)
	require.True(t, summary.Orgs[0].Skipped)
	require.Empty(t, runner.requests, "no Job may be spawned for a no-op")

	records, err := sink.List(context.Background(), "", 0)
	require.NoError(t, err)
	require.Empty(t, records, "hourly no-ops must not bury the audit trail")
}

// The shrink guard: a sweep that would remove too much of the organization
// is refused before any Job exists, and the refusal is itself recorded.
func TestSweepRefusedByShrinkThreshold(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	deps, sink, source := sweepWorld(t, runner)

	// Three members, two of them gone: 2/3 > the 0.5 default.
	store := deps.Mapping.(mapping.Store)
	require.NoError(t, store.Put(context.Background(),
		mapping.Entry{Name: "Also Gone", GitHub: "gone2", Class: mapping.ClassEmployee}))

	// The SOURCE's snapshot, because the sweep refreshes before rendering.
	source.snap.Users = append(source.snap.Users,
		directory.User{Name: "Also Gone", Email: "gone2@example.com", Live: false})

	deps.Orgs["example-org"] = &fakeOrgReader{state: &orgstate.State{
		Org: "example-org",
		Members: []orgstate.Member{
			{Login: "ada", Role: orgstate.RoleMember},
			{Login: "gone", Role: orgstate.RoleMember},
			{Login: "gone2", Role: orgstate.RoleMember},
		},
		TeamMembers: map[string][]string{},
	}}

	summary, err := deps.RunRemovals(context.Background())
	require.NoError(t, err)

	outcome := summary.Orgs[0]
	require.Contains(t, outcome.Error, "threshold")
	require.Contains(t, outcome.Error, "an operator must run this sync")
	require.Empty(t, runner.requests, "no Job may be spawned past the threshold")

	records, err := sink.List(context.Background(), "", 0)
	require.NoError(t, err)
	require.Len(t, records, 1, "the refusal itself must be recorded")
	require.Contains(t, records[0].Error, "threshold")
}

// A person who looks gone according to an unhealthy directory stays.
func TestSweepSparesPeopleOfUnhealthySources(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	deps, _, source := sweepWorld(t, runner)

	// A set whose source now fails, seeded with the last-known-good
	// snapshot that says "gone" is gone: the sweep's own refresh records
	// the failure, the source reports unhealthy, and the renderer must
	// leave its people alone.
	failing := directory.NewSet(deps.Logger, &failingSource{name: "corp"})
	failing.Caches()[0].Set(source.snap)
	deps.Directories = failing

	summary, err := deps.RunRemovals(context.Background())
	require.NoError(t, err)

	require.True(t, summary.Orgs[0].Skipped,
		"with corp unhealthy, its not-live person must not be removed, leaving nothing to do")
	require.Empty(t, runner.requests)
}

type failingSource struct{ name string }

func (f *failingSource) Name() string { return f.name }

func (f *failingSource) Fetch(context.Context) (*directory.Snapshot, error) {
	return nil, errors.New("directory unreachable")
}

// Sweeps are serialized: a second caller is told, not queued.
func TestConcurrentSweepsAreRefused(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	deps, _, _ := sweepWorld(t, runner)

	done := make(chan error, 1)

	go func() {
		_, err := deps.RunRemovals(context.Background())
		done <- err
	}()

	<-runner.started // the first sweep is now inside the reconciler

	_, err := deps.RunRemovals(context.Background())
	require.ErrorIs(t, err, server.ErrRunInProgress)

	close(runner.release)
	require.NoError(t, <-done)
}

// POST /sync on the internal listener triggers a sweep and reports it.
func TestInternalSyncEndpoint(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	deps, _, _ := sweepWorld(t, runner)

	app := server.NewHealthApp(deps)

	req := httptest.NewRequest(http.MethodPost, "/sync", http.NoBody)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second})
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `"gone"`)
	require.Len(t, runner.requests, 1)
}

// Without a reconciler the endpoint says so instead of pretending.
func TestInternalSyncWithoutReconciler(t *testing.T) {
	t.Parallel()

	deps, _, _ := sweepWorld(t, nil)
	deps.Applier = nil

	app := server.NewHealthApp(deps)

	req := httptest.NewRequest(http.MethodPost, "/sync", http.NoBody)

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}

// The main, gateway-routed app must NOT expose the unauthenticated trigger:
// /sync there is the operator page, behind auth.
func TestMainAppDoesNotExposeTheInternalTrigger(t *testing.T) {
	t.Parallel()

	deps, _, _ := sweepWorld(t, &fakeRunner{})
	deps.Auth = &fixedAuth{role: auth.RoleNone}

	app := server.NewApp(deps)

	req := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(""))

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, fiber.StatusUnauthorized, resp.StatusCode,
		"the gateway-routed surface must refuse an unauthenticated sync")
}
