package server

import (
	"bufio"
	"context"
	"fmt"
	"sort"

	"github.com/truvity/github-roster/pkg/audit"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/broker"
	"github.com/truvity/github-roster/pkg/ui"
)

// syncData drives the sync page: the form, a computed plan, an apply
// outcome, and the recent run history.
type syncData struct {
	Orgs []string
	Org  string
	// History is the most recent audit records — every run is one, so
	// the sync page doubles as the reconciliation history.
	History []audit.Record
	// Plan is the broker's computed plan, shown for review. The console
	// renders it and holds nothing but its hash.
	Plan *broker.PlanResponse
	// Applied is the broker's report of an executed apply.
	Applied *broker.ApplyResponse
	// Drift means the plan shown replaced one that went stale between
	// review and apply.
	Drift bool
	Error string
	// NoTeam names mapped people no team claims — every plan silently
	// excludes them, so the sync page says so before showing a plan.
	NoTeam []string
}

// noTeamNames lists mapped people the configuration resolves to no team,
// from the desired-only join — cheap, no GitHub reads. Best-effort: an
// unreadable mapping must not keep the sync form from rendering.
func (d *Deps) noTeamNames(ctx context.Context) []string {
	if d.Mapping == nil {
		return nil
	}

	entries, err := d.Mapping.List(ctx)
	if err != nil {
		return nil
	}

	var names []string

	joined := d.desiredOnlyJoin(entries)
	for i := range joined.People {
		// Live people only — a suspended person is teamless by design
		// (the removals path), same gate as the join's warning and the
		// People-page badge.
		if joined.People[i].NoTeam && joined.People[i].Live {
			names = append(names, joined.People[i].Name)
		}
	}

	sort.Strings(names)

	return names
}

func (d *Deps) handleSync(c fiber.Ctx) error {
	data := syncData{Orgs: d.orgNames(), NoTeam: d.noTeamNames(c.Context())}

	// Best-effort: history must never keep the sync form from rendering.
	if d.Audit != nil {
		if records, err := d.Audit.List(c.Context(), "", historyLimit); err == nil {
			data.History = records
		}
	}

	return d.Renderer.Render(c, fiber.StatusOK, "sync", ui.Page{
		Title:  "Sync",
		Nav:    "sync",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}

// historyLimit bounds the sync page's recent-runs section; the Audit page
// is the full trail.
const historyLimit = 10

// handleSyncPreview asks the broker to compute a plan.
//
// The broker computes desired state itself and stores the plan by content
// hash; this console renders the answer and keeps only the hash. There is
// no Job and no wait: the preview is a read.
func (d *Deps) handleSyncPreview(c fiber.Ctx) error {
	orgName := formValue(c, "org")
	data := syncData{Orgs: d.orgNames(), Org: orgName}

	// Always 200, even for failure banners: Cloudflare replaces 5xx
	// bodies with its own error page, hiding the message the operator
	// actually needs to read.
	render := func() error {
		return d.Renderer.Render(c, fiber.StatusOK, "sync", ui.Page{
			Title: "Sync", Nav: "sync", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	if d.Broker == nil {
		data.Error = "no applier broker is configured in this deployment"

		return render()
	}

	plan, err := d.Broker.Plan(c.Context(), orgName, c.Get(fiber.HeaderAuthorization))
	if err != nil {
		data.Error = err.Error()

		return render()
	}

	data.Plan = plan

	return render()
}

// handleSyncApply asks the broker to execute a reviewed plan, by hash.
//
// The broker recomputes from fresh state and executes only on an exact
// hash match, so the operator's approval covers exactly what runs. On
// drift the fresh plan comes back for another look instead of an error.
func (d *Deps) handleSyncApply(c fiber.Ctx) error {
	orgName := formValue(c, "org")
	hash := formValue(c, "hash")
	data := syncData{Orgs: d.orgNames(), Org: orgName}

	// Always 200, even for failure banners: Cloudflare replaces 5xx
	// bodies with its own error page, hiding the message the operator
	// actually needs to read.
	render := func() error {
		return d.Renderer.Render(c, fiber.StatusOK, "sync", ui.Page{
			Title: "Sync", Nav: "sync", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	if d.Broker == nil {
		data.Error = "no applier broker is configured in this deployment"

		return render()
	}

	if hash == "" {
		data.Error = "no plan hash: compute a plan first"

		return render()
	}

	runID, err := d.Broker.ApplyAsync(c.Context(), orgName, hash, c.Get(fiber.HeaderAuthorization))
	if err != nil {
		data.Error = err.Error()

		return render()
	}

	// The broker is writing to GitHub as we redirect; whatever the
	// console's cache holds is about to be wrong by construction.
	invalidate(d.Orgs[orgName])

	return c.Redirect().To(fmt.Sprintf("/sync/run?org=%s&id=%s&hash=%s", orgName, runID, hash))
}

// handleSyncRun renders the live-run page: the transcript streams into
// it over SSE from the console's proxy endpoint.
func (d *Deps) handleSyncRun(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "run", ui.Page{
		Title:  "Applying",
		Nav:    "sync",
		AuthOn: d.Auth.Enabled(),
		Data: syncRunData{
			Org:  c.Query("org"),
			ID:   c.Query("id"),
			Hash: c.Query("hash"),
		},
	})
}

// syncRunData drives the live-run page.
type syncRunData struct {
	Org  string
	ID   string
	Hash string
}

// handleSyncRunStream proxies the broker's SSE stream. EventSource
// cannot set an Authorization header, so the gateway-authenticated
// console forwards the caller's token on their behalf. The gateway's
// route timeout may cut a long stream; EventSource reconnects and the
// broker replays the transcript, so nothing is lost.
func (d *Deps) handleSyncRunStream(c fiber.Ctx) error {
	if d.Broker == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "no applier broker is configured")
	}

	resp, err := d.Broker.StreamRun(c.Context(), c.Query("org"), c.Query("id"), c.Get(fiber.HeaderAuthorization))
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")

	c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer func() { _ = resp.Body.Close() }()

		buf := make([]byte, 4096)

		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return
				}

				if flushErr := w.Flush(); flushErr != nil {
					return
				}
			}

			if readErr != nil {
				return
			}
		}
	})

	return nil
}

func (d *Deps) orgNames() []string {
	names := make([]string, 0, len(d.Config.Orgs))
	for i := range d.Config.Orgs {
		names = append(names, d.Config.Orgs[i].Name)
	}

	return names
}
