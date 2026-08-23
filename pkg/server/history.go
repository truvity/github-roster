package server

import (
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/ui"
)

// change is one row of the History view: a single membership change (or a
// failure), flattened out of an audit run record. The design's History is
// "one row per change"; the record model is still per-run, so we flatten
// its Adding/Removing here — cause linkage waits for the per-change model.
type change struct {
	At      string
	Org     string
	Kind    audit.Kind
	Verb    string // added | removed | failed
	Subject string // the login, or empty for a run-level failure
	Actor   string // email or "reconciler"/"schedule"
	Detail  string
}

// historyData drives the History page.
type historyData struct {
	Changes []change
	// Filters, echoed back into the form.
	Org  string
	Kind string
	Q    string
	Orgs []string
	// Kinds is the fixed filter vocabulary.
	Kinds []audit.Kind
	Error string
}

func actorOf(r audit.Record) string {
	switch {
	case r.ActorEmail != "":
		return r.ActorEmail
	case r.Actor != "":
		return r.Actor
	default:
		return "operator"
	}
}

// handleHistory renders one row per change, flattened from audit records,
// with org / kind / person filters.
func (d *Deps) handleHistory(c fiber.Ctx) error {
	data := historyData{
		Orgs:  d.orgNames(),
		Org:   c.Query("org"),
		Kind:  c.Query("kind"),
		Q:     strings.ToLower(strings.TrimSpace(c.Query("q"))),
		Kinds: []audit.Kind{audit.KindOperatorSync, audit.KindReconcile, audit.KindRemovals},
	}

	if d.Audit == nil {
		data.Error = "no audit sink is configured in this deployment"

		return d.renderHistory(c, data)
	}

	records, err := d.Audit.List(c.Context(), data.Org, audit.DefaultLimit)
	if err != nil {
		data.Error = err.Error()

		return d.renderHistory(c, data)
	}

	data.Changes = flattenChanges(records, data.Kind, data.Q)

	return d.renderHistory(c, data)
}

// flattenChanges turns per-run audit records into per-change rows, applying
// the kind and person/actor filters. Previews are excluded — history is what
// happened, not what was contemplated.
func flattenChanges(records []audit.Record, kind, q string) []change {
	var out []change

	for i := range records {
		r := records[i]

		if kind != "" && string(r.Kind) != kind {
			continue
		}

		if !r.Confirmed {
			continue
		}

		at := r.At.Format("2006-01-02 15:04Z")
		actor := actorOf(r)

		for _, login := range r.Adding {
			out = append(out, change{At: at, Org: r.Org, Kind: r.Kind, Verb: "added", Subject: login, Actor: actor})
		}

		for _, login := range r.Removing {
			out = append(out, change{At: at, Org: r.Org, Kind: r.Kind, Verb: "removed", Subject: login, Actor: actor})
		}

		if r.Error != "" {
			out = append(out, change{At: at, Org: r.Org, Kind: r.Kind, Verb: "failed", Actor: actor, Detail: r.Error})
		}
	}

	if q != "" {
		filtered := out[:0]
		for _, ch := range out {
			if strings.Contains(strings.ToLower(ch.Subject), q) || strings.Contains(strings.ToLower(ch.Actor), q) {
				filtered = append(filtered, ch)
			}
		}
		out = filtered
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })

	return out
}

func (d *Deps) renderHistory(c fiber.Ctx, data historyData) error {
	return d.Renderer.Render(c, fiber.StatusOK, "history", ui.Page{
		Title:  "History",
		Nav:    "history",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}
