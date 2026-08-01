// Package server is the HTTP surface: the console's server-rendered pages,
// the JSON API, and the separate health listener.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humafiber"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	slogfiber "github.com/samber/slog-fiber"

	"github.com/truvity/github-roster/pkg/applier"
	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/broker"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/runlock"
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
	Orgs map[string]OrgReader
	// Applier spawns reconciler Jobs. Kept for the (gated) unattended
	// removals path until that moves into the broker; the operator sync
	// surface no longer uses it.
	Applier JobRunner
	// Broker is the applier broker's client. Nil where none is
	// configured, in which case the sync surface reports itself
	// unavailable rather than pretending.
	Broker *broker.Client
	// Audit records every run, durably.
	Audit audit.Sink
	// RunLock serializes removals sweeps: ticker, insurance CronJob and
	// any future caller all contend on it, and losers are told rather
	// than queued. Nil is valid and falls back to an in-process lock —
	// correct for exactly one replica; multi-replica installs must wire
	// the Lease implementation (BuildDeps does, when in a cluster).
	RunLock runlock.Lock

	// fallbackLock backs the nil-RunLock case.
	fallbackLock runlock.Memory

	// ApplierApps carries each organization's applier App IDENTIFIERS.
	//
	// Identifiers only, never the key: the web tier reads app id and
	// installation id so it can pass them as Job arguments, and the
	// private key is mounted straight into the Job from a Secret this
	// process never opens. That asymmetry is the whole privilege boundary,
	// so it is visible in the type.
	ApplierApps map[string]ApplierApp
}

// ApplierApp is the non-secret half of an applier App's credentials.
type ApplierApp struct {
	AppID          string
	InstallationID string
}

// OrgReader reads one organization's current GitHub state.
// *orgstate.Reader and *orgstate.Cache satisfy it; tests inject fakes.
type OrgReader interface {
	Read(ctx context.Context) (*orgstate.State, error)
}

// freshReader is the optional cache-bypass. The sync and removals paths
// use it when present: the state an operator confirms, and the state a
// sweep decides removals on, must be the truth right now.
type freshReader interface {
	ReadFresh(ctx context.Context) (*orgstate.State, error)
}

// invalidator is the optional bust-on-write hook, called after every
// applier run — once a Job has written to GitHub, a cached answer is
// wrong by construction.
type invalidator interface {
	Invalidate()
}

// readFresh reads bypassing any cache, falling back to a plain Read for
// readers that have none.
func readFresh(ctx context.Context, reader OrgReader) (*orgstate.State, error) {
	if fresh, ok := reader.(freshReader); ok {
		return fresh.ReadFresh(ctx)
	}

	return reader.Read(ctx)
}

// invalidate drops the reader's cache, when it has one.
func invalidate(reader OrgReader) {
	if cache, ok := reader.(invalidator); ok {
		cache.Invalidate()
	}
}

// JobRunner runs one reconciler Job. *applier.Runner satisfies it.
type JobRunner interface {
	Run(ctx context.Context, req applier.Request) (*applier.Run, error)
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
	// readBufferSize must hold the full request header block. The gateway
	// forwards the OIDC session cookies plus a JWT access token with role
	// assertions — together well past fasthttp's 4 KiB default, which
	// answers 431 before any handler runs.
	readBufferSize = 64 << 10
)

// NewApp builds the console. Exported so tests can drive the whole stack —
// routing, middleware, templates — over a real listener.
func NewApp(deps *Deps) *fiber.App {
	app := fiber.New(fiber.Config{
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
		IdleTimeout:    idleTimeout,
		BodyLimit:      bodyLimit,
		ReadBufferSize: readBufferSize,
		ErrorHandler:   errorHandler(deps),
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
	//
	// The writing routes additionally refuse cross-site form posts. That
	// is not belt-and-braces on top of the bearer token: the gateway in
	// front of this service holds a SESSION COOKIE, so a form posted from
	// another site would arrive with that cookie, be authenticated by the
	// gateway, and reach here indistinguishable from a real click.
	//
	// The guard is STATELESS by hard-won necessity. The token-store CSRF
	// pattern requires every POST to land on the pod that minted the
	// token, and no amount of load-balancer affinity proved reliable —
	// on 2026-08-01 a form GET and its POST landed on different pods
	// straight through a cookie-hash policy, 403ing every operator
	// write. Browsers state a request's provenance themselves
	// (Sec-Fetch-Site, Origin), the gateway guarantees a browser is on
	// the other end, and provenance is the whole question here.

	app.Get("/mapping", requireOperator, deps.handleMapping)
	app.Get("/mapping/edit", requireOperator, deps.handleMappingForm)
	app.Post("/mapping/save", requireOperator, sameOriginOnly, deps.handleMappingSave)
	app.Post("/mapping/delete", requireOperator, sameOriginOnly, deps.handleMappingDelete)
	app.Get("/mapping/import", requireOperator, deps.handleImportForm)
	app.Post("/mapping/import", requireOperator, sameOriginOnly, deps.handleImportPreview)
	app.Post("/mapping/import/apply", requireOperator, sameOriginOnly, deps.handleImportApply)

	app.Get("/sync", requireOperator, deps.handleSync)
	app.Post("/sync/preview", requireOperator, sameOriginOnly, deps.handleSyncPreview)
	app.Post("/sync/apply", requireOperator, sameOriginOnly, deps.handleSyncApply)

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

	// The roster. Consumed by the gitops puller (INF-443), which commits it
	// so cfggen stays reproducible from git alone — so its shape is a
	// contract across repositories, not an implementation detail.
	huma.Register(api, huma.Operation{
		OperationID: "get-roster",
		Method:      http.MethodGet,
		Path:        "/api/roster",
		Summary:     "The joined roster",
		Description: "Directory liveness joined with the mapping and GitHub state. " +
			"Warnings are advisory and never block the response: a roster that " +
			"refused to render because one person is unmapped would be useless " +
			"precisely when it is needed.",
	}, func(ctx context.Context, _ *struct{}) (*rosterResponse, error) {
		joined, err := deps.buildRoster(ctx)
		if err != nil {
			deps.Logger.ErrorContext(ctx, "building the roster failed", slog.Any("error", err))

			return nil, huma.Error500InternalServerError("could not build the roster")
		}

		return &rosterResponse{Body: joined}, nil
	})

	// The audit log as JSON — same records the page shows, for tooling.
	huma.Register(api, huma.Operation{
		OperationID: "list-audit",
		Method:      http.MethodGet,
		Path:        "/api/audit",
		Summary:     "Audit records, newest first",
	}, func(ctx context.Context, in *auditRequest) (*auditResponse, error) {
		if deps.Audit == nil {
			return nil, huma.Error503ServiceUnavailable("no audit sink is configured")
		}

		records, err := deps.Audit.List(ctx, in.Org, in.Limit)
		if err != nil {
			deps.Logger.ErrorContext(ctx, "listing audit records failed", slog.Any("error", err))

			return nil, huma.Error500InternalServerError("could not list audit records")
		}

		return &auditResponse{Body: records}, nil
	})
}

type auditRequest struct {
	Org   string `query:"org" doc:"restrict to one organization"`
	Limit int    `query:"limit" doc:"maximum records, newest first" default:"50"`
}

type auditResponse struct {
	Body []audit.Record
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

	// POST /sync triggers one removals-only sweep NOW. It lives on the
	// internal listener, unauthenticated, and that is a considered
	// decision rather than an omission:
	//
	//   - the insurance CronJob that calls it runs in-cluster and cannot
	//     obtain a gateway token; this port is not routed by the gateway
	//     and the NetworkPolicy scopes it to the namespace;
	//   - the operation is safe by construction: a removals-only render
	//     can only remove people a healthy directory positively reports
	//     gone. Triggering leaver revocation EARLY is not a privilege —
	//     it is the service doing its one unattended job sooner;
	//   - it is serialized (409 when a run is underway), so it cannot be
	//     used to stampede the reconciler.
	app.Post("/sync", func(c fiber.Ctx) error {
		summary, err := deps.RunRemovals(c.Context())

		switch {
		case errors.Is(err, ErrRunInProgress):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
		case err != nil:
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		default:
			return c.JSON(summary)
		}
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

	// The unattended half. Only when a reconciler exists: a console with
	// no cluster has nothing to sweep with, and says so once.
	switch {
	case deps.Applier == nil:
		deps.Logger.Info("scheduled removals disabled: no reconciler configured")
	case deps.Config.Schedule.RemovalsInterval <= 0:
		deps.Logger.Info("scheduled removals disabled: no interval configured")
	default:
		go deps.scheduleLoop(ctx)
	}

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
