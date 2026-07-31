package mapping

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// Action is what applying a planned row would do.
type Action string

const (
	// ActionCreate adds a person who is not in the store.
	ActionCreate Action = "create"
	// ActionUpdate changes an existing entry.
	ActionUpdate Action = "update"
	// ActionUnchanged is a row that matches what is already stored. Kept
	// in the plan rather than dropped: "nothing to do" is an answer an
	// operator wants to see, especially when re-importing a corrected file.
	ActionUnchanged Action = "unchanged"
	// ActionReject is a row that cannot be applied.
	ActionReject Action = "reject"
)

// PlannedRow is one row of an import, and what it would do.
type PlannedRow struct {
	// Line is the 1-based line in the input, so an operator can find the
	// row they need to fix.
	Line int `json:"line"`
	// Entry is the parsed entry. Partially filled when the row was
	// rejected during parsing.
	Entry Entry `json:"entry"`
	// Action is what applying it would do.
	Action Action `json:"action"`
	// Before is the stored entry an update would replace.
	Before *Entry `json:"before,omitempty"`
	// Reason explains a rejection, and describes the change otherwise.
	Reason string `json:"reason,omitempty"`
}

// Plan is a whole import, decided but not applied.
//
// Deciding and applying are separate steps because an operator has to see
// what a paste of seventy lines is about to do before it does it. A plan
// with a single rejected row is still shown in full: the operator fixes
// that row rather than being told "invalid input" about the whole file.
type Plan struct {
	Rows []PlannedRow `json:"rows"`
}

// Counts summarizes a plan for the confirmation page.
func (p *Plan) Counts() (creates, updates, unchanged, rejects int) {
	for i := range p.Rows {
		switch p.Rows[i].Action {
		case ActionCreate:
			creates++
		case ActionUpdate:
			updates++
		case ActionUnchanged:
			unchanged++
		case ActionReject:
			rejects++
		}
	}

	return creates, updates, unchanged, rejects
}

// Applicable reports whether anything would change.
func (p *Plan) Applicable() bool {
	creates, updates, _, _ := p.Counts()

	return creates+updates > 0
}

// HasRejects reports whether any row cannot be applied.
func (p *Plan) HasRejects() bool {
	_, _, _, rejects := p.Counts()

	return rejects > 0
}

// importColumns is the accepted header. A header is required rather than
// inferred: a column order that silently differs between two pastes is how
// a GitHub handle ends up in the abbreviation field.
var importColumns = []string{"name", "github", "k8s", "class", "pinned"}

// ParseCSV reads an import. The first line must be the header above;
// `k8s`, `class` and `pinned` may be empty, and `pinned` holds
// semicolon-separated "<org>/<team>" entries.
func ParseCSV(input string) ([]PlannedRow, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(input)))
	reader.FieldsPerRecord = -1 // checked per row, so one short row does not abort the file
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("no input")
		}

		return nil, fmt.Errorf("read header: %w", err)
	}

	if err := checkHeader(header); err != nil {
		return nil, err
	}

	var rows []PlannedRow

	for line := 2; ; line++ {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}

		if err != nil {
			rows = append(rows, PlannedRow{
				Line:   line,
				Action: ActionReject,
				Reason: "could not be read: " + err.Error(),
			})

			continue
		}

		rows = append(rows, parseRow(line, record))
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("the header is present but there are no rows")
	}

	return rows, nil
}

func checkHeader(header []string) error {
	if len(header) != len(importColumns) {
		return fmt.Errorf("header must be %q, got %d columns",
			strings.Join(importColumns, ","), len(header))
	}

	for i, want := range importColumns {
		if !strings.EqualFold(strings.TrimSpace(header[i]), want) {
			return fmt.Errorf("column %d must be %q, got %q", i+1, want, header[i])
		}
	}

	return nil
}

func parseRow(line int, record []string) PlannedRow {
	row := PlannedRow{Line: line}

	if len(record) != len(importColumns) {
		row.Action = ActionReject
		row.Reason = fmt.Sprintf("expected %d columns, got %d", len(importColumns), len(record))

		return row
	}

	entry := Entry{
		Name:   strings.TrimSpace(record[0]),
		GitHub: strings.TrimSpace(record[1]),
		K8s:    strings.TrimSpace(record[2]),
		Class:  Class(strings.TrimSpace(record[3])),
	}

	// An empty class means employee. Bots are the exception and have to be
	// stated, which is the right way round: forgetting the class must not
	// quietly create something exempt from liveness.
	if entry.Class == "" {
		entry.Class = ClassEmployee
	}

	for _, pinned := range strings.Split(record[4], ";") {
		if pinned = strings.TrimSpace(pinned); pinned != "" {
			entry.Pinned = append(entry.Pinned, pinned)
		}
	}

	row.Entry = entry

	return row
}

// BuildPlan decides what each parsed row would do against the stored
// entries, and rejects the ones that cannot be applied.
//
// Rows are checked against each other as well as against the store, so a
// file that maps two people to one GitHub handle is caught here rather than
// half-applied.
func BuildPlan(rows []PlannedRow, existing []Entry) *Plan {
	stored := make(map[string]Entry, len(existing))
	for i := range existing {
		stored[existing[i].Name] = existing[i]
	}

	// accumulated is what the store would look like as the plan is applied,
	// so uniqueness is checked against earlier rows too.
	accumulated := append([]Entry(nil), existing...)
	seenNames := map[string]int{}

	plan := &Plan{Rows: make([]PlannedRow, 0, len(rows))}

	for i := range rows {
		row := rows[i]

		if row.Action == ActionReject {
			plan.Rows = append(plan.Rows, row)

			continue
		}

		if first, duplicate := seenNames[row.Entry.Name]; duplicate {
			row.Action = ActionReject
			row.Reason = fmt.Sprintf("%q also appears on line %d", row.Entry.Name, first)
			plan.Rows = append(plan.Rows, row)

			continue
		}

		before, exists := stored[row.Entry.Name]

		if err := CheckInvariants(row.Entry, before, accumulated); err != nil {
			row.Action = ActionReject
			row.Reason = err.Error()
			plan.Rows = append(plan.Rows, row)

			continue
		}

		seenNames[row.Entry.Name] = row.Line

		switch {
		case !exists:
			row.Action = ActionCreate
			row.Reason = "new entry"
		case sameEntry(before, row.Entry):
			row.Action = ActionUnchanged
			row.Reason = "already stored"
			row.Before = &before
		default:
			row.Action = ActionUpdate
			row.Reason = describeChange(before, row.Entry)
			row.Before = &before
		}

		if row.Action != ActionUnchanged {
			accumulated = replaceOrAppend(accumulated, row.Entry)
		}

		plan.Rows = append(plan.Rows, row)
	}

	return plan
}

func sameEntry(a, b Entry) bool {
	return a.GitHub == b.GitHub &&
		a.K8s == b.K8s &&
		a.Class == b.Class &&
		strings.Join(a.Pinned, ";") == strings.Join(b.Pinned, ";")
}

// describeChange names what an update would alter, so the confirmation page
// says "github: octocat → monalisa" rather than merely "update".
func describeChange(before, after Entry) string {
	var changes []string

	if before.GitHub != after.GitHub {
		changes = append(changes, fmt.Sprintf("github: %s → %s", before.GitHub, after.GitHub))
	}

	if before.K8s != after.K8s {
		changes = append(changes, fmt.Sprintf("k8s: %s → %s", orDash(before.K8s), orDash(after.K8s)))
	}

	if before.Class != after.Class {
		changes = append(changes, fmt.Sprintf("class: %s → %s", before.Class, after.Class))
	}

	if strings.Join(before.Pinned, ";") != strings.Join(after.Pinned, ";") {
		changes = append(changes, fmt.Sprintf("pinned: %s → %s",
			orDash(strings.Join(before.Pinned, ", ")),
			orDash(strings.Join(after.Pinned, ", "))))
	}

	return strings.Join(changes, "; ")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}

	return s
}

func replaceOrAppend(entries []Entry, entry Entry) []Entry {
	for i := range entries {
		if entries[i].Name == entry.Name {
			entries[i] = entry

			return entries
		}
	}

	return append(entries, entry)
}
