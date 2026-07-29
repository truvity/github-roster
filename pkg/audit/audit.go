// Package audit records what every reconciler run did, durably.
//
// Records go to object storage, never to this process's stdout. That is a
// deliberate correction of a habit rather than a preference: a log line
// scrolls away, is not queryable, and disappears entirely with the pod that
// wrote it. This service changes people's access, so "what happened, when,
// and who asked for it" has to outlive the container that answered.
//
// The sink is an interface with more than one implementation, for the same
// reason the mapping store is: S3 is a good fit today, and nothing above
// this package should have to know that.
package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/truvity/github-roster/pkg/applier"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/peribolos"
)

// Trigger says what caused a run.
type Trigger string

const (
	// TriggerOperator is a human pressing Sync.
	TriggerOperator Trigger = "operator"
	// TriggerSchedule is the unattended, removals-only run.
	TriggerSchedule Trigger = "schedule"
)

// Record is one run, start to finish.
//
// Dry runs are recorded too. A preview that an operator looked at and did
// NOT apply is part of the story — it is the evidence that a change was
// considered and declined, which is exactly what an auditor asks about
// after an incident.
type Record struct {
	// ID is unique and sorts chronologically.
	ID string `json:"id"`
	// At is when the run started.
	At time.Time `json:"at"`
	// Org is the organization acted on.
	Org string `json:"org"`
	// Trigger is what caused it.
	Trigger Trigger `json:"trigger"`
	// Actor is the operator's subject, or empty for a scheduled run.
	Actor string `json:"actor,omitempty"`
	// Mode is what the rendered document was capable of.
	Mode peribolos.Mode `json:"mode"`
	// Confirmed distinguishes a real run from a preview.
	Confirmed bool `json:"confirmed"`

	// Adding and Removing are the computed diff.
	Adding   []string `json:"adding,omitempty"`
	Removing []string `json:"removing,omitempty"`

	// Config is the rendered document, exactly as the Job received it.
	// Stored in full: the diff says what changed, and this says on what
	// basis — without it a record cannot be audited, only believed.
	Config string `json:"config"`

	// Sources is each directory's health at render time, so a reader can
	// tell whether the run acted on fresh data.
	Sources []directory.Status `json:"sources,omitempty"`
	// SkippedSources names directories whose people were left alone.
	SkippedSources []string `json:"skippedSources,omitempty"`
	// Notes are the renderer's explanations.
	Notes []string `json:"notes,omitempty"`

	// Job is the reconciler's own report.
	Job *applier.Run `json:"job,omitempty"`
	// Error is set when the run failed. A failed run is still recorded:
	// the attempt is the thing being audited.
	Error string `json:"error,omitempty"`
}

// Sink stores and retrieves records.
type Sink interface {
	// Write stores one record.
	Write(ctx context.Context, record Record) error
	// List returns records for an organization, newest first. An empty
	// org means every organization.
	List(ctx context.Context, org string, limit int) ([]Record, error)
}

// DefaultLimit is how many records a page shows.
const DefaultLimit = 50

// NewID builds an identifier that sorts chronologically and is safe in an
// object key.
func NewID(at time.Time, suffix string) string {
	return at.UTC().Format("20060102T150405Z") + "-" + sanitize(suffix)
}

// Key is where a record lives under the bucket.
//
// The organization is the first segment so that one bucket policy per
// organization is possible later without moving anything — the TP org
// arrives in M3, and retrofitting a prefix onto existing objects is the
// kind of migration nobody schedules.
func Key(org, id string) string {
	return sanitize(org) + "/" + id + ".json"
}

// OrgFromKey recovers the organization from a key.
func OrgFromKey(key string) string {
	org, _, found := strings.Cut(key, "/")
	if !found {
		return ""
	}

	return org
}

func sanitize(value string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return strings.Trim(b.String(), "-")
}

// FromRun assembles a record from everything a run produced.
func FromRun(trigger Trigger, actor string, result *peribolos.Result, run *applier.Run, sources []directory.Status, runErr error) Record {
	at := time.Now().UTC()
	if run != nil && !run.StartedAt.IsZero() {
		at = run.StartedAt.UTC()
	}

	record := Record{
		ID:      NewID(at, jobSuffix(run)),
		At:      at,
		Trigger: trigger,
		Actor:   actor,
		Sources: sources,
		Job:     run,
	}

	if result != nil {
		record.Org = result.Org
		record.Mode = result.Mode
		record.Adding = result.Adding
		record.Removing = result.Removing
		record.Config = result.YAML
		record.SkippedSources = result.SkippedSources
		record.Notes = result.Notes
	}

	if run != nil {
		record.Confirmed = run.Confirmed
	}

	if runErr != nil {
		record.Error = runErr.Error()
	}

	return record
}

func jobSuffix(run *applier.Run) string {
	if run == nil || run.JobName == "" {
		return "norun"
	}

	return run.JobName
}

// Summary is a one-line description for the list page.
func (r Record) Summary() string {
	what := "dry run"
	if r.Confirmed {
		what = "applied"
	}

	switch {
	case r.Error != "":
		return what + " — failed"
	case len(r.Adding) == 0 && len(r.Removing) == 0:
		return what + " — no changes"
	default:
		return fmt.Sprintf("%s — %d added, %d removed", what, len(r.Adding), len(r.Removing))
	}
}
