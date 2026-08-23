package broker

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/reconciler"
)

// ReconcileStatus is one organization's last reconcile outcome, for the
// Status page. It answers "what does the loop see, and did it act?"
type ReconcileStatus struct {
	Org string `json:"org"`
	// Enabled is the per-org day-0 gate (config). When false the loop
	// computes and reports but never applies.
	Enabled bool `json:"enabled"`
	// At is when this pass ran; Next is the expected next tick.
	At   time.Time `json:"at"`
	Next time.Time `json:"next"`
	// Actions is what desired state differs from GitHub by — what the loop
	// would do (disabled) or did (enabled).
	Actions int `json:"actions"`
	// Applied is true when the loop executed the actions (enabled + a
	// clean plan).
	Applied bool `json:"applied"`
	// Held is set when a guard refused the plan (shrink breaker, owner
	// guard, …); Reason names it. The loop retries next tick.
	Held   bool   `json:"held,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Error is a read/render/execute failure (not a guard refusal).
	Error string `json:"error,omitempty"`
}

// ReconcileStatuses returns a snapshot of the per-org reconcile status,
// for the console's Status page.
func (d *Deps) ReconcileStatuses() []ReconcileStatus {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()

	out := make([]ReconcileStatus, 0, len(d.reconcileStatus))
	for i := range d.Config.Orgs {
		if st, ok := d.reconcileStatus[d.Config.Orgs[i].Name]; ok {
			out = append(out, st)
		}
	}

	return out
}

func (d *Deps) setReconcileStatus(st ReconcileStatus) {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()

	if d.reconcileStatus == nil {
		d.reconcileStatus = map[string]ReconcileStatus{}
	}

	d.reconcileStatus[st.Org] = st
}

// RunReconcile performs one reconcile pass across every organization: it
// computes each org's full desired state and, where the org is enabled,
// applies it. Disabled orgs are computed and reported but never changed —
// the day-0 gate, and the "what it would do" the Status page shows.
//
// It runs INSIDE the broker on the broker's own reads and credential, and
// shares the apply lock with operator applies and the removals sweep: one
// writer at a time, always.
func (d *Deps) RunReconcile(ctx context.Context) error {
	if !d.applyMu.TryLock() {
		return ErrSweepInProgress
	}
	defer d.applyMu.Unlock()

	// Fresh liveness first; failures are non-fatal (last-known-good + the
	// render's fail-safes carry it).
	if d.Directories != nil {
		for source, err := range d.Directories.Refresh(ctx) {
			d.Logger.WarnContext(ctx, "directory refresh failed; using last known good",
				"source", source, "error", err)
		}
	}

	next := time.Now().UTC().Add(d.reconcileInterval())

	for i := range d.Config.Orgs {
		d.reconcileOrg(ctx, &d.Config.Orgs[i], next)
	}

	return nil
}

// reconcileOrg computes one org's desired state and applies it when the
// org is enabled. One org failing never stops the others: each records its
// own status and, when anything is applied, its own audit record.
func (d *Deps) reconcileOrg(ctx context.Context, cfgOrg *config.Org, next time.Time) {
	st := ReconcileStatus{
		Org:     cfgOrg.Name,
		Enabled: cfgOrg.ReconcileEnabled,
		At:      time.Now().UTC(),
		Next:    next,
	}

	org, ok := d.Orgs[cfgOrg.Name]
	if !ok {
		st.Error = "no broker handle configured"
		d.setReconcileStatus(st)

		return
	}

	entry, err := d.compute(ctx, cfgOrg.Name, org)
	if err != nil {
		// A guard refusal (shrink breaker, owner guard) is a hold, not a
		// failure: named, retried next tick, and surfaced to the operator.
		if reconciler.IsGuardError(err) {
			st.Held = true
			st.Reason = err.Error()
		} else {
			st.Error = err.Error()
		}

		d.setReconcileStatus(st)

		return
	}

	st.Actions = len(entry.Plan.Actions)

	// Disabled, or nothing to do: report and stop. A dry pass writes no
	// audit record — the trail records actions, not heartbeats.
	if !cfgOrg.ReconcileEnabled || st.Actions == 0 {
		d.setReconcileStatus(st)

		return
	}

	writer, err := reconciler.NewGitHubWriter(org.Source)
	if err != nil {
		st.Error = err.Error()
		d.recordSweep(ctx, entry, "", err)
		d.setReconcileStatus(st)

		return
	}

	var lines []string

	execErr := reconciler.Execute(ctx, writer, entry.Plan, func(line string) {
		lines = append(lines, line)
	})

	report := reconciler.Report(entry.Plan, execErr == nil)
	d.record(ctx, audit.TriggerSchedule, audit.Actor{Subject: "reconciler"}, entry, report, execErr)

	if execErr != nil {
		st.Error = execErr.Error()
		d.setReconcileStatus(st)

		return
	}

	st.Applied = true
	d.setReconcileStatus(st)

	d.Logger.InfoContext(ctx, "reconcile applied", "org", cfgOrg.Name, "actions", st.Actions)
}

// reconcileInterval is the configured loop interval, or the default.
func (d *Deps) reconcileInterval() time.Duration {
	if d.Config.Reconcile.Interval > 0 {
		return d.Config.Reconcile.Interval
	}

	return 15 * time.Minute
}

// Reconcile runs the continuous loop until ctx ends. Unlike the
// removals-only Schedule, this computes full desired state for every org
// and applies it where the org is enabled; disabled orgs are computed and
// reported (the "would do" the Status page shows), never changed.
func (d *Deps) Reconcile(ctx context.Context) {
	interval := d.reconcileInterval()

	d.Logger.Info("continuous reconcile enabled", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// An initial pass so the Status page is populated right after a
	// rollout instead of blank until the first tick.
	if err := d.RunReconcile(ctx); err != nil && !errors.Is(err, ErrSweepInProgress) {
		d.Logger.ErrorContext(ctx, "initial reconcile pass failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.RunReconcile(ctx); err != nil && !errors.Is(err, ErrSweepInProgress) {
				d.Logger.ErrorContext(ctx, "reconcile pass failed", "error", err)
			}
		}
	}
}

// handleReconcileStatus serves the per-org reconcile status for the
// console's Status page.
func (d *Deps) handleReconcileStatus(c fiber.Ctx) error {
	return c.JSON(d.ReconcileStatuses())
}

// handleRunReconcile triggers one reconcile pass on demand (the Status
// page's "Sync now"). Disabled orgs only recompute their status; enabled
// orgs apply — the same guarantees as the scheduled loop. A pass already
// running answers 409, not a queue.
func (d *Deps) handleRunReconcile(c fiber.Ctx) error {
	err := d.RunReconcile(c.Context())

	switch {
	case errors.Is(err, ErrSweepInProgress):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	case err != nil:
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
	default:
		return c.JSON(d.ReconcileStatuses())
	}
}
