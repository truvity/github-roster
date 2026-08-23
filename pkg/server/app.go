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

// handleApp serves the embedded single-page app. A request for an existing
// built file (the hashed JS/CSS under assets/) returns it; anything else
// returns index.html so the SPA can route it client-side.
func (d *Deps) handleApp(c fiber.Ctx) error {
	name := strings.TrimPrefix(c.Params("*"), "/")

	if name == "" || name == "index.html" {
		return serveAppIndex(c)
	}

	data, err := fs.ReadFile(appDist, name)
	if err != nil {
		// Unknown path: the SPA's own route, not a missing asset.
		return serveAppIndex(c)
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
