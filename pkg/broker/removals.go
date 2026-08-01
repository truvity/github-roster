package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/reconciler"
	"github.com/truvity/github-roster/pkg/roster"
)

// ErrSweepInProgress answers a sweep asked for while one is running. One
// at a time, always: two reconcilers racing over one organization is a
// class of bug this service refuses to have.
var ErrSweepInProgress = errors.New("a removals sweep is already in progress")

// RemovalsSummary is what one sweep did, org by org.
type RemovalsSummary struct {
	StartedAt time.Time    `json:"startedAt"`
	Orgs      []OrgOutcome `json:"orgs"`
}

// OrgOutcome is one organization's part of a sweep.
type OrgOutcome struct {
	Org      string   `json:"org"`
	Removing []string `json:"removing,omitempty"`
	Skipped  bool     `json:"skipped,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// RunRemovals performs one unattended, removals-only sweep across every
// managed organization.
//
// This is the aggressive-automation surface of the whole service, so its
// shape is deliberately minimal: org membership only, removals only, and
// only of people a HEALTHY directory positively reports gone. Everything
// else needs a human; this needs none, because this is the half that
// carries the leaver-revocation SLA. It runs INSIDE the broker: the
// process that holds the write credential acts on its own schedule and
// its own reads, and nothing external can feed it content.
func (d *Deps) RunRemovals(ctx context.Context) (*RemovalsSummary, error) {
	// Applies and sweeps share the one lock: the broker is single-replica
	// and a caller finding a run underway is told, not queued.
	if !d.applyMu.TryLock() {
		return nil, ErrSweepInProgress
	}
	defer d.applyMu.Unlock()

	summary := &RemovalsSummary{StartedAt: time.Now().UTC()}

	// Fresh liveness first. Refresh failures are fine: the caches keep
	// last-known-good and the renderer skips the unhealthy sources'
	// removals — the fail-safe, not this call, is the guard.
	if d.Directories != nil {
		for source, err := range d.Directories.Refresh(ctx) {
			d.Logger.WarnContext(ctx, "directory refresh failed; using last known good",
				"source", source, "error", err)
		}
	}

	for i := range d.Config.Orgs {
		org := &d.Config.Orgs[i]
		summary.Orgs = append(summary.Orgs, d.sweepOrg(ctx, org))
	}

	return summary, nil
}

// sweepOrg runs the removals-only pipeline for one organization. One
// organization failing must not stop the others: each gets its own
// outcome and, where anything happened, its own audit record.
func (d *Deps) sweepOrg(ctx context.Context, cfgOrg *config.Org) OrgOutcome {
	outcome := OrgOutcome{Org: cfgOrg.Name}

	org, ok := d.Orgs[cfgOrg.Name]
	if !ok {
		outcome.Error = "no broker handle configured"

		return outcome
	}

	entries, err := d.Mapping.List(ctx)
	if err != nil {
		outcome.Error = fmt.Sprintf("read mapping: %v", err)

		return outcome
	}

	// Never a cached answer: a removal decided on stale state is exactly
	// the mistake the fail-safes exist to prevent.
	state, err := org.Reader.ReadScoped(ctx, teamNames(cfgOrg))
	if err != nil {
		outcome.Error = fmt.Sprintf("read organization: %v", err)

		return outcome
	}

	in := roster.Inputs{
		Config:  d.Config,
		Entries: entries,
		Orgs:    map[string]*orgstate.State{cfgOrg.Name: state},
		Now:     time.Now(),
	}

	var unhealthy []string

	if d.Directories != nil {
		in.SourceStatuses = d.Directories.Statuses()
		unhealthy = d.Directories.Unhealthy()

		for _, cache := range d.Directories.Caches() {
			if snapshot, ok := cache.Snapshot(); ok {
				in.Snapshots = append(in.Snapshots, snapshot)
			}
		}
	}

	result, err := peribolos.Render(peribolos.Inputs{
		Mode:             peribolos.ModeRemovalsOnly,
		Org:              cfgOrg,
		Roster:           roster.Join(in),
		State:            state,
		UnhealthySources: unhealthy,
	})
	if err != nil {
		outcome.Error = fmt.Sprintf("render: %v", err)
		d.recordSweep(ctx, nil, "", err)

		return outcome
	}

	// The healthy, common case: nobody to remove. Deliberately NOT an
	// audit record — the trail records actions and refusals, not hourly
	// heartbeats.
	if len(result.Removing) == 0 {
		outcome.Skipped = true
		outcome.Reason = "nothing to remove"

		return outcome
	}

	entry := &stored{Org: cfgOrg.Name, Mode: peribolos.ModeRemovalsOnly, Result: result}

	plan, err := reconciler.BuildPlan(result.Document, cfgOrg.Name, state, reconciler.Options{
		Mode:               peribolos.ModeRemovalsOnly,
		MinAdmins:          d.Config.MinAdminsFor(cfgOrg.Name),
		MaxRemovalFraction: d.Config.Schedule.MaxRemovalFraction,
	})
	if err != nil {
		// A refused plan (shrink breaker, owner guard) is exactly what
		// the audit trail must name.
		outcome.Error = err.Error()
		d.recordSweep(ctx, entry, "", err)
		d.Logger.ErrorContext(ctx, "removals sweep REFUSED",
			"org", cfgOrg.Name, "removing", len(result.Removing), "reason", err.Error())

		return outcome
	}

	entry.Plan = plan

	writer, err := reconciler.NewGitHubWriter(org.Source)
	if err != nil {
		outcome.Error = err.Error()
		d.recordSweep(ctx, entry, "", err)

		return outcome
	}

	var lines []string

	execErr := reconciler.Execute(ctx, writer, plan, func(line string) {
		lines = append(lines, line)
	})

	report := reconciler.Report(plan, execErr == nil)

	d.recordSweep(ctx, entry, report, execErr)

	outcome.Removing = result.Removing

	if execErr != nil {
		outcome.Error = execErr.Error()

		return outcome
	}

	d.Logger.InfoContext(ctx, "scheduled removals applied",
		"org", cfgOrg.Name, "removed", result.Removing)

	return outcome
}

// recordSweep writes the audit record for one org's part of a sweep.
func (d *Deps) recordSweep(ctx context.Context, entry *stored, report string, runErr error) {
	if entry == nil {
		entry = &stored{}
	}

	if auditErr := d.record(ctx, audit.TriggerSchedule, "schedule", entry, report, runErr); auditErr != "" {
		d.Logger.ErrorContext(ctx, "AUDIT RECORD LOST for sweep", "org", entry.Org, "error", auditErr)
	}
}

// handleSweep is the insurance CronJob's entry: "sweep now", nothing
// else. Unauthenticated by the same trust model as the console's old
// internal listener — the network policy admits only this deployment's
// pods, and firing early is the service doing its one unattended job
// sooner, not a privilege.
func (d *Deps) handleSweep(c fiber.Ctx) error {
	summary, err := d.RunRemovals(c.Context())

	switch {
	case errors.Is(err, ErrSweepInProgress):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	case err != nil:
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
	default:
		return c.JSON(summary)
	}
}

// Schedule runs the removals ticker until ctx ends. A zero interval
// leaves the unattended path off, and says so once.
func (d *Deps) Schedule(ctx context.Context) {
	interval := d.Config.Schedule.RemovalsInterval
	if interval <= 0 {
		d.Logger.Info("scheduled removals disabled: no interval configured")

		return
	}

	d.Logger.Info("scheduled removals enabled", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := d.RunRemovals(ctx); err != nil && !errors.Is(err, ErrSweepInProgress) {
				d.Logger.ErrorContext(ctx, "scheduled removals sweep failed", "error", err)
			}
		}
	}
}
