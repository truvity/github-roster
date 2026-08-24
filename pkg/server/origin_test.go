package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/server"
)

// The stateless CSRF guard: provenance comes from the browser's own
// declaration, never from a token store a load balancer must respect.
func TestSameOriginGuard(t *testing.T) {
	t.Parallel()

	app := server.NewApp(newDeps(t, doc, &fixedAuth{role: auth.RoleOperator}))

	post := func(headers map[string]string) int {
		req := httptest.NewRequest(http.MethodPost, "/settings/orgs",
			strings.NewReader("org=example"))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationForm)

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := app.Test(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		return resp.StatusCode
	}

	require.NotEqual(t, fiber.StatusForbidden, post(map[string]string{"Sec-Fetch-Site": "same-origin"}))
	require.NotEqual(t, fiber.StatusForbidden, post(map[string]string{"Sec-Fetch-Site": "none"}))
	require.Equal(t, fiber.StatusForbidden, post(map[string]string{"Sec-Fetch-Site": "cross-site"}))
	// Older browser: Origin only. httptest requests carry Host example.com.
	require.NotEqual(t, fiber.StatusForbidden, post(map[string]string{"Origin": "http://example.com"}))
	require.Equal(t, fiber.StatusForbidden, post(map[string]string{"Origin": "https://evil.example"}))
	// No provenance at all: refused — these are browser-only surfaces.
	require.Equal(t, fiber.StatusForbidden, post(nil))
}
