// Package server is the HTTP surface: the console's server-rendered pages,
// the JSON API, and the separate health listener.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humafiber"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	slogfiber "github.com/samber/slog-fiber"

	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/ui"
	"github.com/truvity/github-roster/pkg/version"
)

// Deps are the collaborators the handlers need.
type Deps struct {
	Logger   *slog.Logger
	Config   *config.Config
	Auth     auth.Authenticator
	Renderer *ui.Renderer
	Version  version.Info

	// The read layers. Optional in tests, which drive the pages that do
	// not need them.
	Mapping     mapping.Reader
	Directories *directory.Set
	// Orgs is one read-only GitHub reader per managed organization.
	Orgs map[string]*orgstate.Reader
}

// Timeouts. The console serves small pages to humans; a request slower than
// this is stuck, not slow.
const (
	readTimeout  = 10 * time.Second
	writeTimeout = 60 * time.Second
	idleTimeout  = 120 * time.Second
	// shutdownGrace lets an in-flight sync confirmation finish rather than
	// leaving an operator unsure whether their click landed.
	shutdownGrace = 20 * time.Second
	// bodyLimit bounds a request. The largest thing anyone posts here is a
	// bulk mapping import pasted into a textarea.
	bodyLimit = 4 << 20
)

// NewApp builds the console. Exported so tests can drive the whole stack —
// routing, middleware, templates — over a real listener.
func NewApp(deps *Deps) *fiber.App {
	app := fiber.New(fiber.Config{
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
		BodyLimit:    bodyLimit,
		ErrorHandler: errorHandler(deps),
	})

	// A panic on one page must not take the process down: the scheduled
	// removals run in it, and those are the part with an SLA.
	app.Use(recover.New())
	app.Use(slogfiber.New(deps.Logger))

	// Everything is behind the token check, registered before any route
	// rather than per route — the failure mode of per-route wrapping is a
	// page someone forgets to wrap. There are no sign-in routes to exempt:
	// the gateway runs the login and forwards a token with every request.
	app.Use(deps.Auth.Middleware())

	app.Get("/", deps.handleStructure)

	// The mapping editor and the audit log are operator surfaces: one
	// changes access, the other records who changed it. Both are gated
	// explicitly.
	app.Get("/mapping", requireOperator, deps.handleMapping)
	app.Get("/audit", requireOperator, deps.handleAudit)

	registerAPI(deps, app)

	return app
}

// registerAPI mounts the JSON surface under huma, which generates its
// OpenAPI description from the handler types.
//
// That matters more here than it looks: GET /roster is consumed by a puller
// in another repository, and the plan calls for that schema to stay stable.
// A generated description that drifts when the type drifts is a far better
// contract than a hand-written one that does not.
func registerAPI(deps *Deps, app *fiber.App) {
	cfg := huma.DefaultConfig("github-roster", deps.Version.Version)
	cfg.DocsPath = "/api/docs"
	cfg.OpenAPIPath = "/api/openapi"

	api := humafiber.New(app, cfg)

	huma.Get(api, "/api/version", func(_ context.Context, _ *struct{}) (*versionResponse, error) {
		return &versionResponse{Body: deps.Version}, nil
	})
}

type versionResponse struct {
	Body version.Info
}

// NewHealthApp builds the health listener, deliberately a separate app on a
// separate port: liveness must not depend on the identity provider being
// reachable, and it must not be routable from outside the cluster.
func NewHealthApp(deps *Deps) *fiber.App {
	app := fiber.New(fiber.Config{
		ReadTimeout:  readTimeout,
		WriteTimeout: readTimeout,
		IdleTimeout:  idleTimeout,
	})

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "version": deps.Version.String()})
	})

	return app
}

// errorHandler renders a fiber error as a page for a browser and as JSON for
// anything else. A login-shaped HTML body answering a JSON request is a
// confusing failure at 3am.
func errorHandler(deps *Deps) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		status := fiber.StatusInternalServerError
		message := "internal error"

		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			status = fiberErr.Code
			message = fiberErr.Message
		} else {
			// An unexpected error's text is for the operator reading logs,
			// not for the caller.
			deps.Logger.ErrorContext(c.Context(), "unhandled error",
				slog.String("path", c.Path()),
				slog.Any("error", err))
		}

		if !wantsHTML(c) {
			return c.Status(status).JSON(fiber.Map{"error": message})
		}

		return deps.Renderer.Render(c, status, "error", ui.Page{
			Title:  "Error",
			AuthOn: deps.Auth.Enabled(),
			Data:   ui.ErrorData{Status: status, Message: message},
		})
	}
}

// Run serves both listeners until ctx is canceled.
func Run(ctx context.Context, deps *Deps) error {
	console := NewApp(deps)
	health := NewHealthApp(deps)

	errs := make(chan error, 2)

	go listen(deps.Logger, "console", console, deps.Config.Listen, errs)
	go listen(deps.Logger, "health", health, deps.Config.HealthListen, errs)

	select {
	case <-ctx.Done():
		deps.Logger.Info("shutting down")
	case err := <-errs:
		// One listener dying leaves the process half-alive and lying to its
		// probes; take the whole thing down.
		shutdown(console, health)

		return err
	}

	shutdown(console, health)

	return nil
}

func listen(logger *slog.Logger, name string, app *fiber.App, addr string, errs chan<- error) {
	logger.Info("listening", slog.String("server", name), slog.String("addr", addr))

	if err := app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		errs <- fmt.Errorf("%s server: %w", name, err)
	}
}

func shutdown(apps ...*fiber.App) {
	for _, app := range apps {
		_ = app.ShutdownWithTimeout(shutdownGrace)
	}
}
