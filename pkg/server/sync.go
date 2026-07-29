package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/applier"
	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/ui"
)

// syncData drives the sync page, the preview, and the confirmation.
type syncData struct {
	Orgs []string
	Org  string
	// Result is the rendered plan, shown before anything is applied.
	Result *peribolos.Result
	// Run is the reconciler's own report, present after a dry run or an
	// apply.
	Run   *applier.Run
	Error string
	// AuditError is set when the run happened but could not be recorded.
	// Shown separately from Error because the two mean opposite things: a
	// confirmed run that failed to record has ALREADY changed GitHub, and
	// telling the operator the sync failed would be a lie.
	AuditError string
	// Confirming means the operator is looking at a dry run and may apply
	// it.
	Confirming bool
	CSRF       string
}

func (d *Deps) handleSync(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "sync", ui.Page{
		Title:  "Sync",
		Nav:    "sync",
		AuthOn: d.Auth.Enabled(),
		Data:   syncData{Orgs: d.orgNames(), CSRF: csrfToken(c)},
	})
}

// handleSyncPreview renders the desired state and runs the reconciler
// WITHOUT --confirm.
//
// This is the dry run the design calls the preview: peribolos itself
// reports what it would change, so the operator is reading the reconciler's
// own answer rather than this service's guess about it.
func (d *Deps) handleSyncPreview(c fiber.Ctx) error {
	return d.runSync(c, false)
}

// handleSyncApply repeats the render and runs the reconciler WITH
// --confirm.
//
// The render is repeated rather than carried over from the preview: the
// directory, the mapping and the organization can all have changed in the
// seconds since, and applying a stale plan is how somebody gets removed on
// the strength of data that is no longer true.
func (d *Deps) handleSyncApply(c fiber.Ctx) error {
	return d.runSync(c, true)
}

func (d *Deps) runSync(c fiber.Ctx, confirm bool) error {
	orgName := formValue(c, "org")
	data := syncData{Orgs: d.orgNames(), Org: orgName, CSRF: csrfToken(c)}

	render := func(status int) error {
		return d.Renderer.Render(c, status, "sync", ui.Page{
			Title: "Sync", Nav: "sync", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	if d.Applier == nil {
		data.Error = "no reconciler is configured in this deployment"

		return render(fiber.StatusServiceUnavailable)
	}

	org, ok := d.Config.Org(orgName)
	if !ok {
		data.Error = fmt.Sprintf("%q is not an organization this instance manages", orgName)

		return render(fiber.StatusBadRequest)
	}

	result, err := d.renderFor(c, org)
	if err != nil {
		data.Error = err.Error()

		return render(fiber.StatusInternalServerError)
	}

	data.Result = result

	identity, _ := auth.From(c)

	run, err := d.Applier.Run(c.Context(), applier.Request{
		Result:            result,
		Confirm:           confirm,
		CredentialsSecret: org.ApplierSecretName(),
		AppID:             d.ApplierApps[org.Name].AppID,
		InstallationID:    d.ApplierApps[org.Name].InstallationID,
		RunID:             runID(),
		Actor:             identity.Subject,
	})

	data.Run = run
	data.Confirming = !confirm

	// Record BEFORE reporting, and record failures too — the attempt is
	// the thing being audited, and a run that failed half-way is exactly
	// the one somebody will ask about later.
	data.AuditError = d.record(c, audit.TriggerOperator, identity.Subject, result, run, err)

	if err != nil {
		data.Error = err.Error()

		return render(fiber.StatusInternalServerError)
	}

	d.Logger.InfoContext(c.Context(), "reconciler run finished",
		"org", org.Name, "confirmed", confirm, "job", run.JobName,
		"succeeded", run.Succeeded, "removing", len(result.Removing),
		"adding", len(result.Adding), "actor", identity.Subject)

	return render(fiber.StatusOK)
}

// record writes the audit record for a run.
//
// A write failure never turns a successful run into a failed one: by the
// time this is called the organization has already been changed, and
// reporting otherwise would be worse than the missing record. It is logged
// at error level and surfaced separately in the UI.
func (d *Deps) record(c fiber.Ctx, trigger audit.Trigger, actor string,
	result *peribolos.Result, run *applier.Run, runErr error,
) string {
	if d.Audit == nil {
		return "no audit sink is configured; this run was not recorded"
	}

	var sources []directory.Status
	if d.Directories != nil {
		sources = d.Directories.Statuses()
	}

	record := audit.FromRun(trigger, actor, result, run, sources, runErr)

	if err := d.Audit.Write(c.Context(), record); err != nil {
		d.Logger.ErrorContext(c.Context(), "AUDIT RECORD LOST: the run happened but could not be recorded",
			"org", record.Org, "job", record.ID, "confirmed", record.Confirmed,
			"error", err)

		return "this run was NOT recorded: " + err.Error()
	}

	return ""
}

// renderFor builds the desired state for one organization.
func (d *Deps) renderFor(c fiber.Ctx, org *config.Org) (*peribolos.Result, error) {
	joined, err := d.buildRoster(c.Context())
	if err != nil {
		return nil, err
	}

	reader, ok := d.Orgs[org.Name]
	if !ok {
		return nil, fmt.Errorf("no GitHub reader configured for %q", org.Name)
	}

	state, err := reader.Read(c.Context())
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", org.Name, err)
	}

	var unhealthy []string
	if d.Directories != nil {
		unhealthy = d.Directories.Unhealthy()
	}

	return peribolos.Render(peribolos.Inputs{
		// An operator pressing Sync gets the full desired state; the
		// unattended path renders removals-only, and that difference is
		// the whole safety model.
		Mode:             peribolos.ModeFull,
		Org:              org,
		Roster:           joined,
		State:            state,
		UnhealthySources: unhealthy,
	})
}

func (d *Deps) orgNames() []string {
	names := make([]string, 0, len(d.Config.Orgs))
	for i := range d.Config.Orgs {
		names = append(names, d.Config.Orgs[i].Name)
	}

	return names
}

// runID names the objects one run creates. Time-based and lowercase so it
// is both sortable in kubectl and a legal object name.
func runID() string {
	return strings.ToLower(time.Now().UTC().Format("20060102t150405"))
}
