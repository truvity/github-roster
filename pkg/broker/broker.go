// Package broker is the applier broker: the one long-lived process holding
// the write credential, behind an intent-only API.
//
// The console sends verbs, never content — "plan a sync", "apply the plan
// with this hash". The broker computes desired state itself from the
// directory and the mapping, keeps plans server-side keyed by content
// hash, and applies a plan only when a fresh recomputation still matches
// the hash the operator approved. See docs/architecture/broker.md.
package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/reconciler"
	"github.com/truvity/github-roster/pkg/roster"
)

// Org is one managed organization, as the broker sees it: a reader and a
// writer sharing the applier App's credentials.
type Org struct {
	// Reader reads live state with the applier App.
	Reader *orgstate.Reader
	// Source mints the applier App's installation tokens; the writer is
	// built from it per apply.
	Source *githubapp.TokenSource
}

// Deps is everything the broker's handlers need.
type Deps struct {
	Logger      *slog.Logger
	Config      *config.Config
	Auth        auth.Authenticator
	Mapping     mapping.Store
	Directories *directory.Set
	Orgs        map[string]*Org
	Audit       audit.Sink

	plans *planStore
	runs  *runRegistry
	// applyMu serializes applies. A mutex rather than a distributed lock
	// because the broker is single-replica by design.
	applyMu sync.Mutex
}

// PlanResponse is what the console renders. Content flows OUT of the
// broker only; nothing in it is accepted back on apply except the hash.
type PlanResponse struct {
	Org       string    `json:"org"`
	Mode      string    `json:"mode"`
	Hash      string    `json:"hash"`
	Actions   []Action  `json:"actions"`
	Notes     []string  `json:"notes,omitempty"`
	Report    string    `json:"report"`
	CreatedAt time.Time `json:"createdAt"`
}

type Action struct {
	Kind  string `json:"kind"`
	Login string `json:"login"`
	Team  string `json:"team,omitempty"`
}

// ApplyResponse reports an executed apply.
type ApplyResponse struct {
	Org     string `json:"org"`
	Hash    string `json:"hash"`
	Applied int    `json:"applied"`
	Report  string `json:"report"`
	// AuditError is set when the apply happened but could not be
	// recorded — the two mean opposite things and are never conflated.
	AuditError string `json:"auditError,omitempty"`
}

// Routes mounts the broker API onto a fiber app.
func (d *Deps) Routes(app *fiber.App) {
	d.plans = newPlanStore()
	d.runs = newRunRegistry()

	app.Get("/healthz", func(c fiber.Ctx) error { return c.SendString("ok") })

	// The insurance heartbeat. No token: the network policy admits only
	// this deployment's pods, and "sweep now" carries no content — the
	// same trust model as the console's old internal listener.
	app.Post("/internal/sweep", d.handleSweep)

	v1 := app.Group("/v1", d.Auth.Middleware(), d.requireOperator)
	v1.Post("/orgs/:org/plans", d.handlePlan)
	v1.Get("/orgs/:org/plans/:hash", d.handleGetPlan)
	v1.Post("/orgs/:org/plans/:hash/apply", d.handleApply)
	v1.Post("/orgs/:org/plans/:hash/apply-async", d.handleApplyAsync)
	v1.Get("/orgs/:org/runs/:id/stream", d.handleRunStream)
}

// requireOperator is the broker's whole authorization policy: every API
// verb requires the operator role from a verified token. There is no
// viewer tier here — reading plans is part of deciding to apply them.
func (d *Deps) requireOperator(c fiber.Ctx) error {
	identity, ok := auth.From(c)
	if !ok || identity.Role != auth.RoleOperator {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "operator role required"})
	}

	return c.Next()
}

// auditActor carries the whole identity into the audit record.
func auditActor(identity auth.Identity) audit.Actor {
	return audit.Actor{Subject: identity.Subject, Email: identity.Email, Name: identity.Name}
}

// actorLabel names the human for logs and the audit trail: their email
// when the identity carries one, their name next, and only then the
// bare token subject — a numeric subject in an audit row answers "who"
// with a riddle.
func actorLabel(identity auth.Identity) string {
	switch {
	case identity.Email != "":
		return identity.Email
	case identity.Name != "":
		return identity.Name
	default:
		return identity.Subject
	}
}

func (d *Deps) org(c fiber.Ctx) (*Org, string, error) {
	name := c.Params("org")
	if org, ok := d.Orgs[name]; ok {
		return org, name, nil
	}

	return nil, name, fmt.Errorf("%q is not an organization this broker manages", name)
}

// handlePlan computes a plan from fresh state and stores it by hash.
func (d *Deps) handlePlan(c fiber.Ctx) error {
	org, name, err := d.org(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	entry, err := d.compute(c.Context(), name, org)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	hash := d.plans.Put(entry)

	identity, _ := auth.From(c)
	d.Logger.InfoContext(c.Context(), "plan computed",
		"org", name, "hash", hash, "actions", len(entry.Plan.Actions), "actor", actorLabel(identity))

	return c.JSON(respond(*entry, hash))
}

func (d *Deps) handleGetPlan(c fiber.Ctx) error {
	entry, ok := d.plans.Get(c.Params("hash"))
	if !ok || entry.Org != c.Params("org") {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such plan (it may have expired); compute a new one"})
	}

	return c.JSON(respond(entry, c.Params("hash")))
}

// errDrift marks an apply refused because the world changed under the
// reviewed plan.
var errDrift = errors.New("plan drifted")

// handleApply re-reads, recomputes, and executes only on an exact match.
func (d *Deps) handleApply(c fiber.Ctx) error {
	org, name, err := d.org(c)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	approved := c.Params("hash")
	if _, ok := d.plans.Get(approved); !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such plan (it may have expired); compute a new one"})
	}

	// One apply at a time, globally: two operators approving different
	// plans against one organization is a conversation, not a race.
	d.applyMu.Lock()
	defer d.applyMu.Unlock()

	identity, _ := auth.From(c)

	fresh, applied, report, err := d.apply(c.Context(), name, org, approved, auditActor(identity))

	switch {
	case errors.Is(err, errDrift):
		// The fresh plan is stored and returned so the operator reviews
		// what is true NOW, not an error message.
		hash := d.plans.Put(fresh)

		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "the organization changed since this plan was reviewed; review the fresh plan",
			"plan":  respond(*fresh, hash),
		})
	case err != nil:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	response := ApplyResponse{Org: name, Hash: approved, Applied: applied, Report: report}
	response.AuditError = d.record(c.Context(), audit.TriggerOperator, auditActor(identity), fresh, report, nil)

	d.Logger.InfoContext(c.Context(), "plan applied",
		"org", name, "hash", approved, "actions", applied, "actor", actorLabel(identity))

	return c.JSON(response)
}

// apply recomputes and executes. Returns the fresh entry either way: on
// drift it is the plan to re-review, on success the plan that ran.
func (d *Deps) apply(ctx context.Context, name string, org *Org, approved string, actor audit.Actor,
) (fresh *stored, applied int, report string, err error) {
	fresh, err = d.compute(ctx, name, org)
	if err != nil {
		return nil, 0, "", err
	}

	if Hash(name, fresh.Mode, fresh.Plan) != approved {
		return fresh, 0, "", errDrift
	}

	writer, err := reconciler.NewGitHubWriter(org.Source)
	if err != nil {
		return fresh, 0, "", err
	}

	var lines []string

	execErr := reconciler.Execute(ctx, writer, fresh.Plan, func(line string) {
		lines = append(lines, line)
	})

	report = strings.Join(lines, "\n")

	if execErr != nil {
		// Half-applied is exactly what the audit record must say.
		auditErr := d.record(ctx, audit.TriggerOperator, actor, fresh, report, execErr)
		if auditErr != "" {
			d.Logger.ErrorContext(ctx, "AUDIT RECORD LOST for failed apply", "org", name, "error", auditErr)
		}

		return fresh, len(lines), report, execErr
	}

	return fresh, len(fresh.Plan.Actions), report, nil
}

// compute is the whole read side: mapping + directory + live GitHub state
// with the broker's own credentials, joined, rendered, and diffed.
func (d *Deps) compute(ctx context.Context, name string, org *Org) (*stored, error) {
	cfgOrg, ok := d.Config.Org(name)
	if !ok {
		return nil, fmt.Errorf("%q is not configured", name)
	}

	// A plan is the thing an operator confirms, so every read layer is
	// refreshed here. Directory failures are not fatal: the caches keep
	// last-known-good and the unhealthy list flows into the render, where
	// it suppresses removals — the fail-safe, not this call, is the guard.
	if d.Directories != nil {
		for source, refreshErr := range d.Directories.Refresh(ctx) {
			d.Logger.WarnContext(ctx, "directory refresh failed; using last known good",
				"source", source, "error", refreshErr)
		}
	}

	entries, err := d.Mapping.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("read mapping: %w", err)
	}

	// Scoped: member listings only for the teams the document names. The
	// per-team fan-out dominates a real organization's read time, and
	// teams outside the document are untouchable by construction anyway.
	state, err := org.Reader.ReadScoped(ctx, teamNames(cfgOrg))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}

	in := roster.Inputs{
		Config:  d.Config,
		Entries: entries,
		Orgs:    map[string]*orgstate.State{name: state},
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

	joined := roster.Join(in)

	result, err := peribolos.Render(peribolos.Inputs{
		Mode:             peribolos.ModeFull,
		Org:              cfgOrg,
		Roster:           joined,
		State:            state,
		UnhealthySources: unhealthy,
	})
	if err != nil {
		return nil, err
	}

	plan, err := reconciler.BuildPlan(result.Document, name, state, reconciler.Options{
		Mode:               peribolos.ModeFull,
		MinAdmins:          d.Config.MinAdminsFor(name),
		MaxRemovalFraction: d.Config.Schedule.MaxRemovalFraction,
	})
	if err != nil {
		return nil, err
	}

	return &stored{Org: name, Mode: peribolos.ModeFull, Plan: plan, Result: result}, nil
}

// teamNames lists the document-named teams of one organization — the
// scope of every broker read.
func teamNames(org *config.Org) []string {
	names := make([]string, 0, len(org.Teams))
	for name := range org.Teams {
		names = append(names, name)
	}

	return names
}

// record writes the audit record. A failure never fails the run it
// records; it is returned for the response and logged loudly.
func (d *Deps) record(ctx context.Context, trigger audit.Trigger, actor audit.Actor,
	entry *stored, report string, runErr error,
) string {
	if d.Audit == nil {
		return "no audit sink is configured; this run was not recorded"
	}

	var sources []directory.Status
	if d.Directories != nil {
		sources = d.Directories.Statuses()
	}

	// The audit record's run shape predates the broker; "job name" is the
	// run's identity and stays meaningful as a broker run id.
	run := &audit.Run{
		JobName: "broker-" + time.Now().UTC().Format("20060102t150405"),
		// Every broker record is a real run: previews never record.
		Confirmed: true,
		Succeeded: runErr == nil,
		Output:    report,
		StartedAt: time.Now(),
	}

	record := audit.FromRun(trigger, actor, entry.Result, run, sources, runErr)

	if err := d.Audit.Write(ctx, record); err != nil {
		d.Logger.ErrorContext(ctx, "AUDIT RECORD LOST: the run happened but could not be recorded",
			"org", record.Org, "error", err)

		return "this run was NOT recorded: " + err.Error()
	}

	return ""
}

func respond(entry stored, hash string) PlanResponse {
	actions := make([]Action, 0, len(entry.Plan.Actions))
	for _, a := range entry.Plan.Actions {
		actions = append(actions, Action{Kind: string(a.Kind), Login: a.Login, Team: a.Team})
	}

	notes := append([]string{}, entry.Result.Notes...)
	notes = append(notes, entry.Plan.Notes...)

	return PlanResponse{
		Org:       entry.Org,
		Mode:      string(entry.Mode),
		Hash:      hash,
		Actions:   actions,
		Notes:     notes,
		Report:    reconciler.Report(entry.Plan, false),
		CreatedAt: entry.CreatedAt,
	}
}
