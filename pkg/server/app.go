package server

import (
	"io/fs"
	"mime"
	"path"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/frontend"
)

// appDist is the built SPA rooted at its dist/ directory.
var appDist, _ = fs.Sub(frontend.Assets, "dist")

// handleAppAsset serves one hashed build file from the embedded SPA
// (mounted at /assets/*). Assets are content-hashed, so they cache forever;
// a miss is a real 404 — the SPA's views live in the URL fragment and never
// produce server-side paths.
func (d *Deps) handleAppAsset(c fiber.Ctx) error {
	name := "assets/" + strings.TrimPrefix(c.Params("*"), "/")

	data, err := fs.ReadFile(appDist, name)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "no such asset")
	}

	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		c.Set(fiber.HeaderContentType, ct)
	}

	c.Set(fiber.HeaderCacheControl, "public, max-age=31536000, immutable")

	return c.Send(data)
}

// serveAppIndex returns the SPA shell. index.html is never cached so a new
// build's hashed asset URLs are picked up immediately.
func serveAppIndex(c fiber.Ctx) error {
	data, err := fs.ReadFile(appDist, "index.html")
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "app is not built")
	}

	c.Set(fiber.HeaderContentType, "text/html; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "no-cache")

	return c.Send(data)
}
