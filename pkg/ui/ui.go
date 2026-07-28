// Package ui renders the console's HTML.
//
// Server-rendered, no SPA, no front-end build step. The console shows a
// handful of tables and takes a confirmation; a JavaScript application would
// add a build pipeline, a second set of dependencies to patch, and a second
// place for an authorization bug to live.
package ui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/auth"
)

//go:embed templates/*.html
var templateFS embed.FS

const layoutFile = "templates/layout.html"

// Renderer renders named pages inside the shared layout.
//
// Each page gets its own parsed template set — layout plus that one page —
// rather than one set holding everything. Templates are keyed by name and
// every page defines "content"; in a single set the last one parsed would
// silently win and every page would render the same body.
type Renderer struct {
	pages   map[string]*template.Template
	version string
}

// NewRenderer parses the embedded templates. It fails at startup rather than
// on the first request: a broken template must not reach production as a 500
// on the one page nobody opened during the rollout.
func NewRenderer(version string) (*Renderer, error) {
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}

	pages := make(map[string]*template.Template, len(files))

	for _, file := range files {
		if file == layoutFile {
			continue
		}

		tmpl, err := template.ParseFS(templateFS, layoutFile, file)
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", file, err)
		}

		pages[strings.TrimSuffix(path.Base(file), ".html")] = tmpl
	}

	if len(pages) == 0 {
		return nil, fmt.Errorf("no page templates found")
	}

	return &Renderer{pages: pages, version: version}, nil
}

// Page is what every template receives.
type Page struct {
	Title    string
	Nav      string // which nav entry is current
	Identity auth.Identity
	AuthOn   bool
	Version  string
	Data     any
}

// ErrorData is what the error page renders.
type ErrorData struct {
	Status  int
	Message string
}

// Render writes a page.
//
// The template is executed into a buffer first, so a template error becomes
// an error the caller can still turn into a response — rather than a half-
// written page on the wire with no honest way to finish it.
func (r *Renderer) Render(c fiber.Ctx, status int, name string, page Page) error {
	tmpl, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("unknown page template %q", name)
	}

	identity, _ := auth.From(c)

	page.Identity = identity
	page.Version = r.version

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", page); err != nil {
		return fmt.Errorf("render %q: %w", name, err)
	}

	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)

	return c.Status(status).Send(buf.Bytes())
}
