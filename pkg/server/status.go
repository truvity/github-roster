package server

import (
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
