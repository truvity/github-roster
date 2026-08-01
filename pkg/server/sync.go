package server

import (
	"errors"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/broker"
	"github.com/truvity/github-roster/pkg/ui"
)

// syncData drives the sync page: the form, a computed plan, and an apply
// outcome.
type syncData struct {
	Orgs []string
	Org  string
	// Plan is the broker's computed plan, shown for review. The console
	// renders it and holds nothing but its hash.
	Plan *broker.PlanResponse
	// Applied is the broker's report of an executed apply.
	Applied *broker.ApplyResponse
	// Drift means the plan shown replaced one that went stale between
	// review and apply.
	Drift bool
	Error string
}

func (d *Deps) handleSync(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "sync", ui.Page{
		Title:  "Sync",
		Nav:    "sync",
		AuthOn: d.Auth.Enabled(),
		Data:   syncData{Orgs: d.orgNames()},
	})
}

// handleSyncPreview asks the broker to compute a plan.
//
// The broker computes desired state itself and stores the plan by content
// hash; this console renders the answer and keeps only the hash. There is
// no Job and no wait: the preview is a read.
func (d *Deps) handleSyncPreview(c fiber.Ctx) error {
	orgName := formValue(c, "org")
	data := syncData{Orgs: d.orgNames(), Org: orgName}

	render := func(status int) error {
		return d.Renderer.Render(c, status, "sync", ui.Page{
			Title: "Sync", Nav: "sync", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	if d.Broker == nil {
		data.Error = "no applier broker is configured in this deployment"

		return render(fiber.StatusServiceUnavailable)
	}

	plan, err := d.Broker.Plan(c.Context(), orgName, c.Get(fiber.HeaderAuthorization))
	if err != nil {
		data.Error = err.Error()

		return render(fiber.StatusBadGateway)
	}

	data.Plan = plan

	return render(fiber.StatusOK)
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

	render := func(status int) error {
		return d.Renderer.Render(c, status, "sync", ui.Page{
			Title: "Sync", Nav: "sync", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	if d.Broker == nil {
		data.Error = "no applier broker is configured in this deployment"

		return render(fiber.StatusServiceUnavailable)
	}

	if hash == "" {
		data.Error = "no plan hash: compute a plan first"

		return render(fiber.StatusBadRequest)
	}

	applied, err := d.Broker.Apply(c.Context(), orgName, hash, c.Get(fiber.HeaderAuthorization))

	var drift *broker.ErrDrift

	switch {
	case errors.As(err, &drift):
		data.Drift = true
		data.Plan = drift.Fresh

		return render(fiber.StatusConflict)
	case err != nil:
		data.Error = err.Error()

		return render(fiber.StatusBadGateway)
	}

	// The broker just wrote to GitHub; whatever the console's cache holds
	// is now wrong by construction.
	invalidate(d.Orgs[orgName])

	data.Applied = applied

	return render(fiber.StatusOK)
}

func (d *Deps) orgNames() []string {
	names := make([]string, 0, len(d.Config.Orgs))
	for i := range d.Config.Orgs {
		names = append(names, d.Config.Orgs[i].Name)
	}

	return names
}
