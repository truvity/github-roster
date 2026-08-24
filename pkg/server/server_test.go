package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/neilotoole/slogt/v2"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/server"
	"github.com/truvity/github-roster/pkg/ui"
	"github.com/truvity/github-roster/pkg/version"
)

const doc = `
oidc: {disabled: true}
companies:
  example:
    directory:
      ssmPrefix: /secrets/directory/example
      domains: [example.com, alt.example.com]
    github:
      org: example
      consoleAppSSM: /secrets/roster/console/example
      applierAppSSM: /secrets/roster/applier/example
      teams:
        team-engineers:
          groups: [team-engineers@example.com, team-engineers@alt.example.com]
        robots:
          pinned: true
`

// fixedAuth authenticates every request as the given role, so routing and
// the role gate can be tested without standing up a provider — the provider
// path has its own tests in pkg/auth.
type fixedAuth struct{ role auth.Role }

func (f *fixedAuth) Enabled() bool { return true }

func (f *fixedAuth) Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		if !f.role.CanView() {
			return fiber.NewError(fiber.StatusUnauthorized, "unauthenticated")
		}

		c.Locals(auth.ContextKey{}, auth.Identity{Subject: "test", Name: "Test Person", Role: f.role})

		return c.Next()
	}
}

func newDeps(t *testing.T, document string, authenticator auth.Authenticator) *server.Deps {
	t.Helper()

	cfg, err := config.Parse([]byte(document))
	require.NoError(t, err)

	renderer, err := ui.NewRenderer("test")
	require.NoError(t, err)

	return &server.Deps{
		Logger:   slogt.New(t),
		Config:   cfg,
		Auth:     authenticator,
		Renderer: renderer,
		Version:  version.Info{Version: "1.2.3"},
		// The in-memory store enforces the same invariants as the real
		// one, so the editor's behavior here is the behavior in
		// production minus the network.
		Mapping: mapping.NewMemory(),
	}
}

func get(t *testing.T, app *fiber.App, path string) (status int, body string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMETextHTML)

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(raw)
}

// The root path now lands on the SPA: the server-rendered pages are retired.
func TestRootRedirectsToApp(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleViewer}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/app", resp.Header.Get("Location"))
}

// The App-manifest routes are the surviving operator-gated surface (the old
// SSR operator pages are retired); requireOperator still refuses a viewer.
func TestOperatorRoutesRejectViewers(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleViewer}))

	status, _ := get(t, app, "/settings/orgs/create-app?org=example")
	require.Equal(t, fiber.StatusForbidden, status)
}

func TestOperatorRoutesAdmitOperators(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleOperator}))

	status, _ := get(t, app, "/settings/orgs/create-app?org=example")
	require.Equal(t, fiber.StatusOK, status)
}

// Every route goes through the middleware, registered once before any route.
// That is what makes it true by construction rather than by remembering.
func TestUnauthenticatedCallersReachNothing(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleNone}))

	for _, path := range []string{"/", "/api/version", "/api/roster", "/settings/orgs/create-app"} {
		status, _ := get(t, app, path)
		require.Equal(t, fiber.StatusUnauthorized, status, "GET %s unauthenticated", path)
	}
}

// The gateway forwards the OIDC session cookies plus a JWT access token
// with role assertions — a header block far past fasthttp's 4 KiB default
// read buffer, which answers 431 before any handler runs. The config must
// leave room for it.
func TestGatewaySizedHeadersAreAccepted(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleViewer}))

	req := httptest.NewRequest(http.MethodGet, "/api/version", http.NoBody)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMETextHTML)
	req.Header.Set(fiber.HeaderCookie, "BearerToken="+strings.Repeat("t", 16<<10))
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+strings.Repeat("j", 8<<10))

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAPIVersion(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleViewer}))

	req := httptest.NewRequest(http.MethodGet, "/api/version", http.NoBody)

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `"version":"1.2.3"`)
}

// The OpenAPI description is the contract the roster puller in another
// repository reads. It has to actually be served.
func TestOpenAPIIsPublished(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleViewer}))

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", http.NoBody)

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "/api/version")
}

// An error answering a JSON request must be JSON. A login-shaped HTML body
// parsed as data is a confusing failure at 3am.
func TestErrorsMatchTheCallersContentType(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleViewer}))

	t.Run("browser gets a page", func(t *testing.T) {
		status, body := get(t, app, "/settings/orgs/create-app?org=example")
		require.Equal(t, fiber.StatusForbidden, status)
		require.Contains(t, body, "<!DOCTYPE html>")
		require.Contains(t, body, "operator role required")
	})

	t.Run("api client gets json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/settings/orgs/create-app?org=example", http.NoBody)

		resp, err := app.Test(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
		require.JSONEq(t, `{"error":"operator role required"}`, string(body))
	})
}

// The health listener is a separate app on a separate port: liveness must
// not depend on the identity provider being reachable, nor on a token.
func TestHealthNeedsNoToken(t *testing.T) {
	t.Parallel()

	app := server.NewHealthApp(newDeps(t, doc, &fixedAuth{role: auth.RoleNone}))

	status, body := get(t, app, "/healthz")

	require.Equal(t, fiber.StatusOK, status)
	require.Contains(t, body, `"version":"1.2.3"`)
}

// A panic on one page must not take the process down: the scheduled removals
// run in it, and those are the part with an SLA.
func TestPanicBecomes500(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &panicAuth{}))

	status, _ := get(t, app, "/")
	require.Equal(t, fiber.StatusInternalServerError, status)
}

type panicAuth struct{}

func (p *panicAuth) Enabled() bool { return true }

func (p *panicAuth) Middleware() fiber.Handler {
	return func(fiber.Ctx) error { panic("boom") }
}
