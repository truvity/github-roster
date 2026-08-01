package server

import (
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// sameOriginOnly rejects a state-changing request whose browser declares
// cross-site provenance — the stateless CSRF guard.
//
// Precedence: Sec-Fetch-Site when present (every current browser sends
// it), the Origin header otherwise. A request carrying neither is refused:
// the writing routes are browser-only surfaces behind the gateway's login,
// and a client that hides its provenance does not get to change the
// mapping. The JSON API and the health listener are separate surfaces and
// carry no form posts.
func sameOriginOnly(c fiber.Ctx) error {
	if site := c.Get("Sec-Fetch-Site"); site != "" {
		// "none" is a user-initiated navigation (address bar, bookmark);
		// browsers never produce it for a scripted cross-site post.
		if site == "same-origin" || site == "none" {
			return c.Next()
		}

		return fiber.NewError(fiber.StatusForbidden, "cross-site request refused")
	}

	origin := c.Get(fiber.HeaderOrigin)
	if origin == "" {
		return fiber.NewError(fiber.StatusForbidden,
			"request declares no provenance (no Origin, no Sec-Fetch-Site)")
	}

	parsed, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(parsed.Host, c.Hostname()) {
		return fiber.NewError(fiber.StatusForbidden, "cross-site request refused")
	}

	return c.Next()
}
