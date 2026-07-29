package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/truvity/github-roster/pkg/applier"
	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/roster"
)

// ErrRunInProgress is returned when a removals run is asked for while one
// is already running. One at a time, always: two concurrent reconcilers
// racing over the same organization is a class of bug this service refuses
// to have.
var ErrRunInProgress = errors.New("a removals run is already in progress")

// RemovalsSummary is what one sweep did, org by org.
type RemovalsSummary struct {
	StartedAt time.Time    `json:"startedAt"`
	Orgs      []OrgOutcome `json:"orgs"`
}

// OrgOutcome is one organization's part of a sweep.
type OrgOutcome struct {
	Org string `json:"org"`
	// Removing lists who this sweep removed (or would have).
	Removing []string `json:"removing,omitempty"`
	// Skipped is true when nothing was done, with Reason saying why.
	// "Nothing to remove" is the normal, healthy case.
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// JobName is set when a reconciler Job ran.
	JobName string `json:"jobName,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RunRemovals performs one unattended, removals-only sweep across every
// managed organization.
//
// This is the aggressive-automation surface of the whole service, so its
// shape is deliberately minimal: org membership only, removals only, and
// only of people a HEALTHY directory positively reports gone. Everything
// else in the service needs a human; this needs none, because this is the
// half that carries the leaver-revocation SLA.
func (d *Deps) RunRemovals(ctx context.Context) (*RemovalsSummary, error) {
	// TryLock, not Lock: a caller finding a run in progress should be told
	// so, not queued behind it to run a second sweep nobody asked for.
	if !d.removalsMu.TryLock() {
		return nil, ErrRunInProgress
	}
	defer d.removalsMu.Unlock()

	if d.Applier == nil {
		return nil, fmt.Errorf("no reconciler is configured; removals cannot run")
	}

	summary := &RemovalsSummary{StartedAt: time.Now().UTC()}

	// Fresh liveness first. A sweep acting on hour-old data would be
	// within the SLA either way, but there is no reason to be stale on
	// purpose. Refresh failures are fine: the caches keep last-known-good
	// and the renderer skips the unhealthy sources' removals.
	if d.Directories != nil {
		d.Directories.Refresh(ctx)
	}

	joined, err := d.buildRoster(ctx)
	if err != nil {
		return nil, fmt.Errorf("build roster: %w", err)
	}

	for i := range d.Config.Orgs {
		org := &d.Config.Orgs[i]
		summary.Orgs = append(summary.Orgs, d.sweepOrg(ctx, org, joined))
	}

	return summary, nil
}

// sweepOrg runs the removals-only pipeline for one organization. One
// organization failing must not stop the others: each gets its own outcome
// and, where anything happened, its own audit record.
func (d *Deps) sweepOrg(ctx context.Context, org *config.Org, joined *roster.Roster) OrgOutcome {
	outcome := OrgOutcome{Org: org.Name}

	reader, ok := d.Orgs[org.Name]
	if !ok {
		outcome.Error = "no GitHub reader configured"

		return outcome
	}

	state, err := reader.Read(ctx)
	if err != nil {
		outcome.Error = fmt.Sprintf("read organization: %v", err)
		d.recordSweep(ctx, nil, nil, err)

		return outcome
	}

	var unhealthy []string
	if d.Directories != nil {
		unhealthy = d.Directories.Unhealthy()
	}

	result, err := peribolos.Render(peribolos.Inputs{
		Mode:             peribolos.ModeRemovalsOnly,
		Org:              org,
		Roster:           joined,
		State:            state,
		UnhealthySources: unhealthy,
	})
	if err != nil {
		outcome.Error = fmt.Sprintf("render: %v", err)
		d.recordSweep(ctx, nil, nil, err)

		return outcome
	}

	// The healthy, common case: nobody to remove. Deliberately NOT an
	// audit record — the audit trail records actions and refusals, not
	// hourly heartbeats, and 8760 no-op records a year per org would bury
	// the entries that matter.
	if len(result.Removing) == 0 {
		outcome.Skipped = true
		outcome.Reason = "nothing to remove"

		return outcome
	}

	// The service-side shrink guard. peribolos has its own
	// (--maximum-removal-delta, driven by the same number), but refusing
	// HERE means no Job is spawned at all and the audit record names the
	// refusal in this service's own terms. A directory returning nonsense
	// convincingly is the scenario; a run that would shrink the org past
	// the threshold needs a human, full stop.
	if refused := d.shrinkRefusal(result, state); refused != "" {
		outcome.Error = refused
		refusedErr := errors.New(refused)
		d.recordSweep(ctx, result, nil, refusedErr)
		d.Logger.ErrorContext(ctx, "removals sweep REFUSED by shrink threshold",
			slog.String("org", org.Name),
			slog.Int("removing", len(result.Removing)),
			slog.String("reason", refused))

		return outcome
	}

	run, err := d.Applier.Run(ctx, applier.Request{
		Result: result,
		// Confirmed without a human: this mode can only remove people a
		// healthy directory positively reports gone, which is exactly the
		// automation the SLA requires.
		Confirm:           true,
		CredentialsSecret: org.ApplierSecretName(),
		AppID:             d.ApplierApps[org.Name].AppID,
		InstallationID:    d.ApplierApps[org.Name].InstallationID,
		RunID:             runID() + "-" + org.Name,
		Actor:             "schedule",
	})

	d.recordSweep(ctx, result, run, err)

	outcome.Removing = result.Removing

	if run != nil {
		outcome.JobName = run.JobName
	}

	if err != nil {
		outcome.Error = err.Error()

		return outcome
	}

	d.Logger.InfoContext(ctx, "scheduled removals applied",
		slog.String("org", org.Name),
		slog.Any("removed", result.Removing),
		slog.String("job", run.JobName))

	return outcome
}

// shrinkRefusal returns a refusal reason, or "" when the sweep may proceed.
func (d *Deps) shrinkRefusal(result *peribolos.Result, state *orgstate.State) string {
	threshold := d.Config.Schedule.MaxRemovalFraction
	if threshold <= 0 {
		return ""
	}

	// The denominator is everyone occupying a non-owner seat — the same
	// population the diff was computed against.
	seats := 0

	for _, member := range state.Members {
		if member.Role != orgstate.RoleAdmin {
			seats++
		}
	}

	for _, invite := range state.Invitations {
		if invite.Login != "" {
			seats++
		}
	}

	if seats == 0 {
		return ""
	}

	fraction := float64(len(result.Removing)) / float64(seats)
	if fraction <= threshold {
		return ""
	}

	return fmt.Sprintf(
		"refusing to remove %d of %d members (%.0f%% > %.0f%% threshold); an operator must run this sync",
		len(result.Removing), seats, fraction*100, threshold*100)
}

// recordSweep writes the audit record for one org's part of a sweep.
func (d *Deps) recordSweep(ctx context.Context, result *peribolos.Result, run *applier.Run, runErr error) {
	if d.Audit == nil {
		return
	}

	record := audit.FromRun(audit.TriggerSchedule, "", result, run, d.sourceStatuses(), runErr)

	if err := d.Audit.Write(ctx, record); err != nil {
		d.Logger.ErrorContext(ctx, "AUDIT RECORD LOST: the sweep happened but could not be recorded",
			slog.String("id", record.ID), slog.Any("error", err))
	}
}

// scheduleLoop runs RunRemovals every configured interval until ctx ends.
//
// The first sweep happens one interval after startup rather than at boot: a
// crash-looping pod must not hammer GitHub, and POST /sync exists for "now".
func (d *Deps) scheduleLoop(ctx context.Context) {
	interval := d.Config.Schedule.RemovalsInterval

	d.Logger.Info("scheduled removals enabled", slog.Duration("interval", interval))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			summary, err := d.RunRemovals(ctx)
			if err != nil {
				// ErrRunInProgress here means an operator-triggered or
				// insurance-triggered run is underway, which is fine.
				d.Logger.WarnContext(ctx, "scheduled removals sweep did not run", slog.Any("error", err))

				continue
			}

			for _, outcome := range summary.Orgs {
				if outcome.Error != "" {
					d.Logger.ErrorContext(ctx, "scheduled removals sweep failed for org",
						slog.String("org", outcome.Org), slog.String("error", outcome.Error))
				}
			}
		}
	}
}
