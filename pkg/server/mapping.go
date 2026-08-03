package server

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/roster"
	"github.com/truvity/github-roster/pkg/ui"
)

// mappingListData is the editor's index. It carries the join alongside the
// raw entries so each row can trace the full identity chain — IdP(s) →
// mapping → the editor list, with compact per-person status: one
// liveness dot per IdP and one standing badge per org. The FULL trace
// stays on the person detail page.
type mappingListData struct {
	Entries []mapping.Entry
	Flash   string
	// People indexes the joined roster by name for the status columns;
	// nil when the read layers are not wired (tests). Best-effort — a
	// broken directory must not take the editor down.
	People      map[string]roster.Person
	SourceNames []string
	OrgNames    []string
}

func (d *Deps) handleMapping(c fiber.Ctx) error {
	if d.Mapping == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "no mapping store configured")
	}

	entries, err := d.Mapping.List(c.Context())
	if err != nil {
		return fmt.Errorf("list mapping: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	data := mappingListData{Entries: entries, Flash: c.Query("flash")}

	if joined, err := d.buildRoster(c.Context()); err == nil {
		data.People = make(map[string]roster.Person, len(joined.People))
		for i := range joined.People {
			data.People[joined.People[i].Name] = joined.People[i]
		}
	}

	if d.Directories != nil {
		for _, s := range d.Directories.Statuses() {
			data.SourceNames = append(data.SourceNames, s.Source)
		}

		sort.Strings(data.SourceNames)
	}

	for i := range d.Config.Orgs {
		data.OrgNames = append(data.OrgNames, d.Config.Orgs[i].Name)
	}

	sort.Strings(data.OrgNames)

	return d.Renderer.Render(c, fiber.StatusOK, "mapping", ui.Page{
		Title:  "People",
		Nav:    "mapping",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}

// mappingFormData drives both the create and the edit form, and the
// confirmation step.
type mappingFormData struct {
	Entry mapping.Entry
	// Before is the stored entry when editing, nil when creating.
	Before *mapping.Entry
	// Pinned is the raw comma-separated field, preserved across a failed
	// submission so an operator does not retype it.
	Pinned string
	// Emails is the raw comma-separated field, same treatment.
	Emails string
	// EmailsFetched notes that the emails shown were fetched from the
	// IdP(s) by name rather than typed, so the confirmation says so.
	EmailsFetched bool
	// Suggestions are the directory people, for the name datalist —
	// adding a person usually starts from someone the IdP already knows.
	Suggestions []directorySuggestion
	// Error is a validation failure to show above the form.
	Error string
	// Confirming switches the template from form to confirmation.
	Confirming bool
	// Change describes what confirming would do.
	Change string
}

// directorySuggestion is one directory person offered by the add form.
type directorySuggestion struct {
	Name   string
	Emails string
	// K8s is the conventional namespace abbreviation derived from the
	// name — first initial, dash, last name — offered as a prefill, never
	// imposed: the operator sees and can change it before the review.
	K8s string
}

// abbrevKeep strips everything a namespace abbreviation cannot carry.
var abbrevKeep = regexp.MustCompile(`[^a-z0-9-]+`)

// conventionalAbbrev renders "Oleg Tsarev" as "otsar" — the emp-{slug5}
// convention the fleet's namespaces follow: first initial plus the first
// four letters of the last name (otsar, ktere, aprok).
func conventionalAbbrev(name string) string {
	fields := strings.Fields(strings.ToLower(name))
	if len(fields) == 0 {
		return ""
	}

	abbrev := fields[0]
	if len(fields) > 1 {
		first := []rune(fields[0])
		last := []rune(fields[len(fields)-1])
		if len(last) > 4 {
			last = last[:4]
		}

		abbrev = string(first[:1]) + string(last)
	}

	abbrev = abbrevKeep.ReplaceAllString(abbrev, "")
	if len(abbrev) > mapping.MaxAbbrevLen {
		abbrev = abbrev[:mapping.MaxAbbrevLen]
	}

	return strings.Trim(abbrev, "-")
}

// directorySuggestions lists everyone the cached snapshots know, so the
// form can offer names and the save step can fetch emails by name.
func (d *Deps) directorySuggestions() []directorySuggestion {
	if d.Directories == nil {
		return nil
	}

	byName := map[string][]string{}

	for _, cache := range d.Directories.Caches() {
		snapshot, ok := cache.Snapshot()
		if !ok {
			continue
		}

		for _, user := range snapshot.Users {
			if user.Name == "" || user.Email == "" {
				continue
			}

			byName[user.Name] = append(byName[user.Name], strings.ToLower(user.Email))
		}
	}

	out := make([]directorySuggestion, 0, len(byName))
	for name, emails := range byName {
		sort.Strings(emails)
		out = append(out, directorySuggestion{
			Name:   name,
			Emails: strings.Join(emails, ", "),
			K8s:    conventionalAbbrev(name),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

func (d *Deps) handleMappingForm(c fiber.Ctx) error {
	data := mappingFormData{Suggestions: d.directorySuggestions()}

	if name := c.Query("name"); name != "" {
		entry, err := d.Mapping.Get(c.Context(), name)
		if err != nil {
			if errors.Is(err, mapping.ErrNotFound) {
				return fiber.NewError(fiber.StatusNotFound, "no mapping entry named "+name)
			}

			return err
		}

		data.Entry = entry
		data.Before = &entry
		data.Pinned = strings.Join(entry.Pinned, ", ")
		data.Emails = strings.Join(entry.Emails, ", ")
	} else {
		data.Entry.Class = mapping.ClassEmployee
	}

	return d.Renderer.Render(c, fiber.StatusOK, "mapping_form", ui.Page{
		Title:  "Mapping",
		Nav:    "mapping",
		AuthOn: d.Auth.Enabled(),
		Data:   data,
	})
}

// handleMappingSave validates, then either asks for confirmation or writes.
//
// The confirmation step is not ceremony. Every field here decides somebody's
// access, and the store's invariants deliberately make some of it
// irreversible — an abbreviation cannot be reassigned once a namespace
// exists under it. Showing the change before making it is the cheapest
// possible guard.
func (d *Deps) handleMappingSave(c fiber.Ctx) error {
	entry := mapping.Entry{
		Name:   formValue(c, "name"),
		GitHub: formValue(c, "github"),
		K8s:    formValue(c, "k8s"),
		Class:  mapping.Class(formValue(c, "class")),
	}

	pinnedRaw := formValue(c, "pinned")
	for _, pinned := range strings.Split(pinnedRaw, ",") {
		if pinned = strings.TrimSpace(pinned); pinned != "" {
			entry.Pinned = append(entry.Pinned, pinned)
		}
	}

	emailsRaw := formValue(c, "emails")
	for _, email := range strings.Split(emailsRaw, ",") {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			entry.Emails = append(entry.Emails, email)
		}
	}

	emailsFetched := false

	// Left empty, the IdP answers: the directories are the authority on
	// which addresses a person exists under, so fetch by name rather
	// than make the operator retype what the platform already knows.
	if len(entry.Emails) == 0 {
		for _, suggestion := range d.directorySuggestions() {
			if suggestion.Name == entry.Name {
				for _, email := range strings.Split(suggestion.Emails, ", ") {
					if email != "" {
						entry.Emails = append(entry.Emails, email)
					}
				}

				emailsFetched = len(entry.Emails) > 0
			}
		}
	}

	data := mappingFormData{
		Entry:         entry,
		Pinned:        pinnedRaw,
		Emails:        strings.Join(entry.Emails, ", "),
		EmailsFetched: emailsFetched,
	}

	before, err := d.Mapping.Get(c.Context(), entry.Name)
	if err != nil && !errors.Is(err, mapping.ErrNotFound) {
		return err
	}

	if err == nil {
		data.Before = &before
	}

	all, err := d.Mapping.List(c.Context())
	if err != nil {
		return err
	}

	// Validate before showing the confirmation, so an operator is never
	// asked to confirm something that cannot be applied.
	if err := mapping.CheckInvariants(entry, before, all); err != nil {
		data.Error = err.Error()

		return d.Renderer.Render(c, fiber.StatusUnprocessableEntity, "mapping_form", ui.Page{
			Title: "Mapping", Nav: "mapping", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	if c.FormValue("confirm") != "yes" {
		data.Confirming = true
		data.Change = describe(data.Before, entry)

		return d.Renderer.Render(c, fiber.StatusOK, "mapping_form", ui.Page{
			Title: "Mapping", Nav: "mapping", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	if err := d.writeEntry(c, entry); err != nil {
		return err
	}

	flash := saved(data.Before, entry)

	// The 2026-08-03 lesson: a person no team claims produces
	// hash-identical sync plans, and nothing said why. Say it at the
	// moment the entry is written, when the operator can still fix it
	// in the same sitting.
	if d.entryHasNoTeam(entry) {
		flash += url.QueryEscape(" — WARNING: matches no team; no sync will include them until a group, members: entry or pin does")
	}

	return c.Redirect().To("/mapping?flash=" + flash)
}

// entryHasNoTeam answers "would any team claim this entry" from the
// configuration and the directory caches alone — no GitHub reads, cheap
// enough for a form submit. False on any doubt: a warning that can cry
// wolf gets ignored.
func (d *Deps) entryHasNoTeam(entry mapping.Entry) bool {
	joined := d.desiredOnlyJoin([]mapping.Entry{entry})

	for i := range joined.People {
		if joined.People[i].Name == entry.Name {
			// Same live-only gate as every other no-team surface.
			return joined.People[i].NoTeam && joined.People[i].Live
		}
	}

	return false
}

// desiredOnlyJoin runs the roster join without GitHub state: enough to
// resolve desired teams (config × directories × mapping), which is all
// the no-team surfaces need.
func (d *Deps) desiredOnlyJoin(entries []mapping.Entry) *roster.Roster {
	in := roster.Inputs{
		Config:  d.Config,
		Entries: entries,
		Orgs:    map[string]*orgstate.State{},
		Now:     time.Now(),
	}

	if d.Directories != nil {
		in.SourceStatuses = d.Directories.Statuses()

		for _, cache := range d.Directories.Caches() {
			if snapshot, ok := cache.Snapshot(); ok {
				in.Snapshots = append(in.Snapshots, snapshot)
			}
		}
	}

	return roster.Join(in)
}

func (d *Deps) writeEntry(c fiber.Ctx, entry mapping.Entry) error {
	store, ok := d.Mapping.(mapping.Store)
	if !ok {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the mapping store is read-only in this deployment")
	}

	if err := store.Put(c.Context(), entry); err != nil {
		return err
	}

	identity, _ := auth.From(c)
	d.Logger.InfoContext(c.Context(), "mapping entry written",
		"name", entry.Name, "github", entry.GitHub, "actor", identity.Subject)

	return nil
}

func (d *Deps) handleMappingDelete(c fiber.Ctx) error {
	store, ok := d.Mapping.(mapping.Store)
	if !ok {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the mapping store is read-only in this deployment")
	}

	name := formValue(c, "name")
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}

	if err := store.Delete(c.Context(), name); err != nil {
		return err
	}

	identity, _ := auth.From(c)
	d.Logger.InfoContext(c.Context(), "mapping entry deleted", "name", name, "actor", identity.Subject)

	return c.Redirect().To("/mapping?flash=deleted")
}

// importData drives the bulk import page and its confirmation.
type importData struct {
	// Input is the pasted text, preserved so a rejected paste can be
	// corrected rather than retyped.
	Input string
	Plan  *mapping.Plan
	// Counts, precomputed because a template cannot call a multi-value
	// method.
	Creates, Updates, Unchanged, Rejects int
	Error                                string
}

func (d *Deps) handleImportForm(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "mapping_import", ui.Page{
		Title:  "Bulk import",
		Nav:    "mapping",
		AuthOn: d.Auth.Enabled(),
		Data:   importData{},
	})
}

// handleImportPreview parses and plans, but writes nothing.
//
// Seventy pasted lines are exactly the case where "apply and see" is
// unacceptable, so planning and applying are separate requests with the
// plan shown in between.
func (d *Deps) handleImportPreview(c fiber.Ctx) error {
	input := formValue(c, "csv")
	data := importData{Input: input}

	rows, err := mapping.ParseCSV(input)
	if err != nil {
		data.Error = err.Error()

		return d.Renderer.Render(c, fiber.StatusUnprocessableEntity, "mapping_import", ui.Page{
			Title: "Bulk import", Nav: "mapping", AuthOn: d.Auth.Enabled(), Data: data,
		})
	}

	existing, err := d.Mapping.List(c.Context())
	if err != nil {
		return err
	}

	data.Plan = mapping.BuildPlan(rows, existing)
	data.Creates, data.Updates, data.Unchanged, data.Rejects = data.Plan.Counts()

	return d.Renderer.Render(c, fiber.StatusOK, "mapping_import", ui.Page{
		Title: "Bulk import", Nav: "mapping", AuthOn: d.Auth.Enabled(), Data: data,
	})
}

// handleImportApply re-parses and re-plans the same input rather than
// trusting a plan round-tripped through the browser.
//
// The store can have changed between preview and confirm, and a plan
// posted back from a form is attacker-controlled input. Re-planning costs
// one read and removes both problems; rejected rows are skipped, so a file
// with one bad line still applies the rest.
func (d *Deps) handleImportApply(c fiber.Ctx) error {
	store, ok := d.Mapping.(mapping.Store)
	if !ok {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the mapping store is read-only in this deployment")
	}

	input := formValue(c, "csv")

	rows, err := mapping.ParseCSV(input)
	if err != nil {
		return fiber.NewError(fiber.StatusUnprocessableEntity, err.Error())
	}

	existing, err := d.Mapping.List(c.Context())
	if err != nil {
		return err
	}

	plan := mapping.BuildPlan(rows, existing)
	identity, _ := auth.From(c)

	var applied int

	for i := range plan.Rows {
		row := plan.Rows[i]

		if row.Action != mapping.ActionCreate && row.Action != mapping.ActionUpdate {
			continue
		}

		if err := store.Put(c.Context(), row.Entry); err != nil {
			return fmt.Errorf("line %d (%s): %w", row.Line, row.Entry.Name, err)
		}

		applied++
	}

	d.Logger.InfoContext(c.Context(), "bulk import applied",
		"entries", applied, "rows", len(plan.Rows), "actor", identity.Subject)

	return c.Redirect().To("/mapping?flash=imported")
}

// formValue reads a form field and COPIES it.
//
// fiber is zero-allocation: the string it returns points into the fasthttp
// request buffer, which is recycled the moment the handler returns. Keeping
// such a string — in the mapping store, a cache, anything outliving the
// request — leaves a pointer into memory that the next request overwrites.
//
// It is not a theoretical hazard. Without the copy, an entry saved as
// k8s="ada" read back as k8s="Ala" — the first bytes of the NEXT request's
// "Alan Turing". Silent, data-dependent corruption of the field that names
// a person's namespace.
func formValue(c fiber.Ctx, key string) string {
	return strings.Clone(strings.TrimSpace(c.FormValue(key)))
}

// describe renders the change a save would make, for the confirmation.
func describe(before *mapping.Entry, after mapping.Entry) string {
	if before == nil {
		return "Create " + after.Name + " → " + after.GitHub
	}

	var changes []string

	if before.GitHub != after.GitHub {
		changes = append(changes, "github: "+before.GitHub+" → "+after.GitHub)
	}

	if strings.Join(before.Emails, ",") != strings.Join(after.Emails, ",") {
		changes = append(changes, "emails: "+dash(strings.Join(before.Emails, ", "))+
			" → "+dash(strings.Join(after.Emails, ", ")))
	}

	if before.K8s != after.K8s {
		changes = append(changes, "k8s: "+dash(before.K8s)+" → "+dash(after.K8s))
	}

	if before.Class != after.Class {
		changes = append(changes, "class: "+string(before.Class)+" → "+string(after.Class))
	}

	if strings.Join(before.Pinned, ",") != strings.Join(after.Pinned, ",") {
		changes = append(changes, "pinned: "+dash(strings.Join(before.Pinned, ", "))+
			" → "+dash(strings.Join(after.Pinned, ", ")))
	}

	if len(changes) == 0 {
		return "No change."
	}

	return strings.Join(changes, "; ")
}

func dash(s string) string {
	if s == "" {
		return "—"
	}

	return s
}

func saved(before *mapping.Entry, entry mapping.Entry) string {
	if before == nil {
		return "created+" + entry.Name
	}

	return "updated+" + entry.Name
}
