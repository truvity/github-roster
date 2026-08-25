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
	// Paused is the operator's runtime switch (config-independent): a
	// paused org is computed and reported but never applied, even if
	// enabled. Distinct from the day-0 Enabled gate.
	Paused bool `json:"paused,omitempty"`
	// At is when this pass ran; Next is the expected next tick.
	At   time.Time `json:"at"`
	Next time.Time `json:"next"`
	// Actions is what desired state differs from GitHub by — what the loop
	// would do (disabled) or did (enabled). Adds/Removes/RoleChanges/
	// TeamChanges break that total down so the operator sees WHAT would
	// change, not just how many.
	Actions     int `json:"actions"`
	Adds        int `json:"adds,omitempty"`
	Removes     int `json:"removes,omitempty"`
	RoleChanges int `json:"roleChanges,omitempty"`
	TeamChanges int `json:"teamChanges,omitempty"`
	// Details is the per-action list behind the counts, so the console can
	// explain "+2 invite" as the two logins and teams it stands for.
	Details []ReconcileChange `json:"details,omitempty"`
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

// ReconcileChange is one concrete action a plan would take, surfaced so the
// console's Status view can expand a count into the who and which team.
type ReconcileChange struct {
	Verb  string `json:"verb"`
	Login string `json:"login"`
	Team  string `json:"team,omitempty"`
}

// changeVerb renders an action kind as the short verb the console shows.
func changeVerb(kind reconciler.ActionKind, admin bool) string {
	switch kind {
	case reconciler.ActionAddMember:
		return "invite"
	case reconciler.ActionAddAdmin:
		return "invite-admin"
	case reconciler.ActionRemoveMember:
		return "remove"
	case reconciler.ActionCancelInvite:
		return "cancel-invite"
	case reconciler.ActionSetRole:
		if admin {
			return "role→owner"
		}

		return "role→member"
	case reconciler.ActionTeamAdd:
		return "team-add"
	case reconciler.ActionTeamRemove:
		return "team-remove"
	default:
		return string(kind)
	}
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

	// Pick up operator-added directories before reading liveness, so a new
	// directory is fetched in this same pass.
	d.reloadDirectories(ctx)

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
	for i := range entry.Plan.Actions {
		a := &entry.Plan.Actions[i]
		switch a.Kind {
		case reconciler.ActionAddMember, reconciler.ActionAddAdmin:
			st.Adds++
		case reconciler.ActionRemoveMember, reconciler.ActionCancelInvite:
			st.Removes++
		case reconciler.ActionSetRole:
			st.RoleChanges++
		case reconciler.ActionTeamAdd, reconciler.ActionTeamRemove:
			st.TeamChanges++
		}

		st.Details = append(st.Details, ReconcileChange{
			Verb:  changeVerb(a.Kind, a.Admin),
			Login: a.Login,
			Team:  a.Team,
		})
	}

	if d.Control != nil {
		if paused, perr := d.Control.Paused(ctx, cfgOrg.Name); perr != nil {
			d.Logger.WarnContext(ctx, "reconcile: pause flag unreadable; assuming not paused",
				"org", cfgOrg.Name, "error", perr)
		} else {
			st.Paused = paused
		}

		// The operator's UI enable/disable decision overrides the config
		// day-0 default when set, so a born-disabled org can be turned on
		// (or off) from the console after its dry-run is reviewed.
		if override, oerr := d.Control.EnabledOverride(ctx, cfgOrg.Name); oerr != nil {
			d.Logger.WarnContext(ctx, "reconcile: enabled override unreadable; using config default",
				"org", cfgOrg.Name, "error", oerr)
		} else if override != nil {
			st.Enabled = *override
		}
	}

	// Disabled, paused, or nothing to do: report and stop. A dry pass
	// writes no audit record — the trail records actions, not heartbeats.
	if !st.Enabled || st.Paused || st.Actions == 0 {
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

// reconcileRunTimeout bounds one triggered pass. Generous: a pass reads
// every directory and every org; the ticker loop imposes no bound of its
// own and a stuck pass holds the apply lock either way, so this is a
// backstop against a wedged network call, not a pacing device.
const reconcileRunTimeout = 10 * time.Minute

// handleRunReconcile triggers one reconcile pass on demand (the Status
// page's "Sync now") and returns AT ONCE: a full pass reads every
// directory and every organization (tens of seconds), which outlives the
// gateway's route timeout — the synchronous version answered 200 to a
// browser that had already been handed a 504. The trigger is the contract
// now: the pass runs in the background with the same guarantees as the
// scheduled loop, and the caller watches Status until the run lands. A
// pass already running answers 409, not a queue.
func (d *Deps) handleRunReconcile(c fiber.Ctx) error {
	// Peek at the apply lock so an operator gets the honest 409 up front;
	// the background pass re-acquires it. A trigger slipping between the
	// two locks just finds the lock taken and logs ErrSweepInProgress —
	// the same outcome, one tick later.
	if !d.applyMu.TryLock() {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": ErrSweepInProgress.Error()})
	}
	d.applyMu.Unlock()

	go func() { //nolint:gosec // G118: deliberately DETACHED from the request context — outliving the trigger request is the point of the async hand-off
		ctx, cancel := context.WithTimeout(context.Background(), reconcileRunTimeout)
		defer cancel()

		if err := d.RunReconcile(ctx); err != nil && !errors.Is(err, ErrSweepInProgress) {
			d.Logger.ErrorContext(ctx, "triggered reconcile pass failed", "error", err)
		}
	}()

	return c.JSON(fiber.Map{"started": true})
}

func (d *Deps) setPaused(c fiber.Ctx, paused bool) error {
	if d.Control == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "no control store configured"})
	}

	org := c.Params("org")
	if _, ok := d.Config.Org(org); !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "unknown organization"})
	}

	if err := d.Control.SetPaused(c.Context(), org, paused); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
	}

	return c.JSON(fiber.Map{"org": org, "paused": paused})
}

// handlePause pauses an organization's reconcile loop.
func (d *Deps) handlePause(c fiber.Ctx) error { return d.setPaused(c, true) }

// handleUnpause resumes it.
func (d *Deps) handleUnpause(c fiber.Ctx) error { return d.setPaused(c, false) }

// setEnabled writes the operator's enable/disable decision for an org.
func (d *Deps) setEnabled(c fiber.Ctx, enabled bool) error {
	if d.Control == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "no control store configured"})
	}

	org := c.Params("org")
	if _, ok := d.Config.Org(org); !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "unknown organization"})
	}

	if err := d.Control.SetEnabled(c.Context(), org, enabled); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
	}

	return c.JSON(fiber.Map{"org": org, "enabled": enabled})
}

// handleEnable turns the reconcile loop on for an org.
func (d *Deps) handleEnable(c fiber.Ctx) error { return d.setEnabled(c, true) }

// handleDisable turns it off.
func (d *Deps) handleDisable(c fiber.Ctx) error { return d.setEnabled(c, false) }
