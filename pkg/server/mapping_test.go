package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/server"
)

// editor drives the mapping editor the way a browser does: it keeps the
// CSRF cookie and token between requests.
type editor struct {
	t     *testing.T
	app   *fiber.App
	store mapping.Store
}

func newEditor(t *testing.T, role auth.Role) *editor {
	t.Helper()

	deps := newDeps(t, doc, &fixedAuth{role: role})

	store, ok := deps.Mapping.(mapping.Store)
	require.True(t, ok)

	return &editor{t: t, app: server.NewApp(deps), store: store}
}

// save posts a confirmed entry the way the UI does.
func (e *editor) save(name, github, k8s string) *http.Response {
	e.t.Helper()

	form := entryForm(name, github, k8s, "employee")
	form.Set("confirm", "yes")

	return e.post("/mapping/save", form, true)
}

// post submits a form. sameOrigin=false simulates a cross-site submission
// — a browser posting our form from somebody else's page.
func (e *editor) post(path string, form url.Values, sameOrigin bool) *http.Response {
	e.t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationForm)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMETextHTML)

	if sameOrigin {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	} else {
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set(fiber.HeaderOrigin, "https://evil.example")
	}

	resp, err := e.app.Test(req)
	require.NoError(e.t, err)

	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return string(data)
}

func entryForm(name, github, k8s, class string) url.Values {
	return url.Values{
		"name": {name}, "github": {github}, "k8s": {k8s}, "class": {class}, "pinned": {""},
	}
}

// The gateway in front of this service holds a session cookie, so a form
// posted from another site would arrive authenticated. CSRF is the only
// thing standing between that and a write.
func TestWritesRequireACSRFToken(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleOperator)

	form := entryForm("Ada Lovelace", "ada", "ada", "employee")
	form.Set("confirm", "yes")

	resp := e.post("/mapping/save", form, false)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	_ = resp.Body.Close()

	_, err := e.store.Get(context.Background(), "Ada Lovelace")
	require.ErrorIs(t, err, mapping.ErrNotFound, "nothing may be written without a token")
}

// A first submission shows what would happen and writes nothing.
func TestSaveConfirmsBeforeWriting(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleOperator)

	resp := e.post("/mapping/save", entryForm("Ada Lovelace", "ada", "ada", "employee"), true)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	page := body(t, resp)
	require.Contains(t, page, "Confirm")
	require.Contains(t, page, "Create Ada Lovelace")

	_, err := e.store.Get(context.Background(), "Ada Lovelace")
	require.ErrorIs(t, err, mapping.ErrNotFound, "the preview must not write")
}

func TestSaveWritesOnConfirmation(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleOperator)

	resp := e.save("Ada Lovelace", "ada", "ada")
	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)
	_ = resp.Body.Close()

	entry, err := e.store.Get(context.Background(), "Ada Lovelace")
	require.NoError(t, err)
	require.Equal(t, "ada", entry.GitHub)
	require.Equal(t, mapping.ClassEmployee, entry.Class)
}

// The store's invariants must reach the operator as a message, not a 500.
func TestSaveSurfacesInvariantFailures(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleOperator)

	resp := e.save("Ada Lovelace", "ada", "ada")
	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)
	_ = resp.Body.Close()

	// Same abbreviation, different person.
	clash := entryForm("Alan Turing", "alan", "ada", "employee")

	resp = e.post("/mapping/save", clash, true)
	require.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
	require.Contains(t, body(t, resp), "already taken")
}

// Never asked to confirm something that cannot be applied.
func TestInvalidInputIsRejectedBeforeConfirmation(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleOperator)

	resp := e.post("/mapping/save", entryForm("Ada Lovelace", "ada", "NOT_DNS", "employee"), true)

	require.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)

	page := body(t, resp)
	require.Contains(t, page, "DNS-1123")
	require.NotContains(t, page, "Confirm")
}

func TestViewersCannotReachTheEditor(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleViewer)

	for _, path := range []string{"/mapping", "/mapping/edit"} {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		req.Header.Set(fiber.HeaderAccept, fiber.MIMETextHTML)

		resp, err := e.app.Test(req)
		require.NoError(t, err)
		require.Equal(t, fiber.StatusForbidden, resp.StatusCode, "GET %s as viewer", path)
		_ = resp.Body.Close()
	}
}

func TestDeleteRemovesTheEntry(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleOperator)

	_ = e.save("Ada Lovelace", "ada", "ada").Body.Close()

	resp := e.post("/mapping/delete", url.Values{"name": {"Ada Lovelace"}}, true)
	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)
	_ = resp.Body.Close()

	_, err := e.store.Get(context.Background(), "Ada Lovelace")
	require.ErrorIs(t, err, mapping.ErrNotFound)
}

// TestStoredValuesSurviveLaterRequests is a regression test for fiber's
// zero-allocation model.
//
// c.FormValue returns a string pointing into the fasthttp request buffer,
// which is recycled when the handler returns. Without copying, an entry
// saved as k8s="ada" read back as k8s="Ala" — the first bytes of the NEXT
// request's "Alan Turing". Silent, data-dependent corruption of the field
// that names somebody's namespace, and invisible until a second request
// with a longer value happens to reuse the buffer.
func TestStoredValuesSurviveLaterRequests(t *testing.T) {
	t.Parallel()

	e := newEditor(t, auth.RoleOperator)

	_ = e.save("Ada Lovelace", "ada", "ada").Body.Close()

	// Several further requests, with longer values, to reuse the buffers.
	for _, name := range []string{"Alan Turing", "Grace Hopper", "Barbara Liskov"} {
		_ = e.post("/mapping/save", entryForm(name, "someone-else", "", "employee"), true).Body.Close()
	}

	entry, err := e.store.Get(context.Background(), "Ada Lovelace")
	require.NoError(t, err)

	require.Equal(t, "Ada Lovelace", entry.Name)
	require.Equal(t, "ada", entry.GitHub)
	require.Equal(t, "ada", entry.K8s, "a stored value must not be a window onto a recycled request buffer")
	require.Equal(t, mapping.ClassEmployee, entry.Class)
}
