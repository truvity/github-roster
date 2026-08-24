package server

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/auth"
)

// requireOperator refuses a viewer reaching a route that changes something.
// It survives the retirement of the server-rendered pages because the
// GitHub App-manifest routes are still operator-gated.
func requireOperator(c fiber.Ctx) error {
	identity, ok := auth.From(c)
	if !ok || !identity.Role.CanOperate() {
		return fiber.NewError(fiber.StatusForbidden, "operator role required")
	}

	return c.Next()
}

// wantsHTML reports whether the caller prefers HTML — the error handler uses
// it to answer a browser with a page and an API client with JSON.
func wantsHTML(c fiber.Ctx) bool {
	return strings.Contains(c.Get(fiber.HeaderAccept), fiber.MIMETextHTML)
}
