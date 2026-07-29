package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/ui"
)

// mappingListData is the editor's index.
type mappingListData struct {
	Entries []mapping.Entry
	Flash   string
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

	return d.Renderer.Render(c, fiber.StatusOK, "mapping", ui.Page{
		Title:  "Mapping",
		Nav:    "mapping",
		AuthOn: d.Auth.Enabled(),
		Data:   mappingListData{Entries: entries, Flash: c.Query("flash")},
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
	// Error is a validation failure to show above the form.
	Error string
	// Confirming switches the template from form to confirmation.
	Confirming bool
	// Change describes what confirming would do.
	Change string
	CSRF   string
}

func (d *Deps) handleMappingForm(c fiber.Ctx) error {
	data := mappingFormData{CSRF: csrfToken(c)}

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

	data := mappingFormData{Entry: entry, Pinned: pinnedRaw, CSRF: csrfToken(c)}

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

	return c.Redirect().To("/mapping?flash=" + saved(data.Before, entry))
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
	CSRF                                 string
}

func (d *Deps) handleImportForm(c fiber.Ctx) error {
	return d.Renderer.Render(c, fiber.StatusOK, "mapping_import", ui.Page{
		Title:  "Bulk import",
		Nav:    "mapping",
		AuthOn: d.Auth.Enabled(),
		Data:   importData{CSRF: csrfToken(c)},
	})
}

// handleImportPreview parses and plans, but writes nothing.
//
// Seventy pasted lines are exactly the case where "apply and see" is
// unacceptable, so planning and applying are separate requests with the
// plan shown in between.
func (d *Deps) handleImportPreview(c fiber.Ctx) error {
	input := formValue(c, "csv")
	data := importData{Input: input, CSRF: csrfToken(c)}

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
