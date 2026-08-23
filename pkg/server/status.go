package server

import (
	"context"
	"net/url"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/broker"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/ui"
)

// statusData drives the Status page: the reconcile loop's per-organization
// view — is it enabled, what does it see, did it act, is anything held.
type statusData struct {
	// Sources is each directory's health — the read side of the loop's
	// world. A stale source suppresses that source's removals, so it
	// belongs on the same page as the loop's per-org outcome.
	Sources  []directory.Status
	Statuses []broker.ReconcileStatus
	Flash    string
	Error    string
}

// handleStatus renders the reconcile loop's per-org status from the broker.
func (d *Deps) handleStatus(c fiber.Ctx) error {
	data := statusData{}

	if d.Directories != nil {
		data.Sources = d.Directories.Statuses()
	}

	render := func() error {
		return d.Renderer.Render(c, fiber.StatusOK, "status", ui.Page{
			Title:  "Status",
			Nav:    "status",
			AuthOn: d.Auth.Enabled(),
			Data:   data,
		})
	}

	if d.Broker == nil {
		data.Error = "no applier broker is configured in this deployment"

		return render()
	}

	statuses, err := d.Broker.ReconcileStatus(c.Context(), c.Get(fiber.HeaderAuthorization))
	if err != nil {
		data.Error = err.Error()

		return render()
	}

	data.Statuses = statuses

	return render()
}

// handleStatusSync triggers an on-demand reconcile pass and returns to the
// Status page. Disabled orgs only recompute; nothing is applied unattended
// by this beyond what an enabled org's loop would already do.
func (d *Deps) handleStatusSync(c fiber.Ctx) error {
	if d.Broker == nil {
		return c.Redirect().To("/status")
	}

	if err := d.Broker.RunReconcile(c.Context(), c.Get(fiber.HeaderAuthorization)); err != nil {
		return c.Redirect().To("/status?flash=" + url.QueryEscape("reconcile could not run: "+err.Error()))
	}

	return c.Redirect().To("/status?flash=" + url.QueryEscape("reconcile pass complete"))
}

// reconcileAfterEdit fires a best-effort reconcile pass after an operator
// mapping edit so the change takes effect promptly rather than at the next
// tick — the design's "the loop also runs on an operator edit". Non-blocking
// (the save has already succeeded) and detached from the request context so
// it is not canceled when the response returns; failures are logged only.
func (d *Deps) reconcileAfterEdit(c fiber.Ctx) {
	if d.Broker == nil {
		return
	}

	token := c.Get(fiber.HeaderAuthorization)
	ctx := context.WithoutCancel(c.Context())

	go func() {
		if err := d.Broker.RunReconcile(ctx, token); err != nil {
			d.Logger.ErrorContext(ctx, "reconcile after mapping edit failed", "error", err)
		}
	}()
}

// handleStatusControl pauses or resumes one organization's reconcile loop.
func (d *Deps) handleStatusControl(c fiber.Ctx) error {
	if d.Broker == nil {
		return c.Redirect().To("/status")
	}

	org := c.FormValue("org")
	paused := c.FormValue("action") == "pause"

	if err := d.Broker.SetPaused(c.Context(), org, paused, c.Get(fiber.HeaderAuthorization)); err != nil {
		return c.Redirect().To("/status?flash=" + url.QueryEscape("could not change pause state: "+err.Error()))
	}

	verb := "resumed"
	if paused {
		verb = "paused"
	}

	return c.Redirect().To("/status?flash=" + url.QueryEscape(org+" "+verb))
}
