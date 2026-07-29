package mapping_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/mapping"
)

const header = "name,github,k8s,class,pinned\n"

func parse(t *testing.T, body string) []mapping.PlannedRow {
	t.Helper()

	rows, err := mapping.ParseCSV(header + body)
	require.NoError(t, err)

	return rows
}

func TestParseCSV(t *testing.T) {
	t.Parallel()

	rows := parse(t, "Ada Lovelace,ada,ada,employee,\nExample Bot,ex-bot,,bot,org/robots;org/auditor\n")

	require.Len(t, rows, 2)

	require.Equal(t, 2, rows[0].Line)
	require.Equal(t, "Ada Lovelace", rows[0].Entry.Name)
	require.Equal(t, mapping.ClassEmployee, rows[0].Entry.Class)
	require.Empty(t, rows[0].Entry.Pinned)

	require.Equal(t, mapping.ClassBot, rows[1].Entry.Class)
	require.Equal(t, []string{"org/robots", "org/auditor"}, rows[1].Entry.Pinned)
}

// Forgetting the class must not quietly create something exempt from
// liveness, so the default is the governed one.
func TestEmptyClassDefaultsToEmployee(t *testing.T) {
	t.Parallel()

	rows := parse(t, "Ada Lovelace,ada,ada,,\n")
	require.Equal(t, mapping.ClassEmployee, rows[0].Entry.Class)
}

// A column order that silently differs between two pastes is how a GitHub
// handle ends up in the abbreviation field.
func TestHeaderIsRequiredAndChecked(t *testing.T) {
	t.Parallel()

	_, err := mapping.ParseCSV("name,github,class,k8s,pinned\nAda,ada,employee,ada,\n")
	require.ErrorContains(t, err, "column 3")

	_, err = mapping.ParseCSV("name,github\nAda,ada\n")
	require.ErrorContains(t, err, "header must be")

	_, err = mapping.ParseCSV("")
	require.ErrorContains(t, err, "no input")

	_, err = mapping.ParseCSV(header)
	require.ErrorContains(t, err, "no rows")
}

// One malformed row must not abort the file — the operator fixes that row.
func TestShortRowIsRejectedButOthersSurvive(t *testing.T) {
	t.Parallel()

	rows := parse(t, "Ada Lovelace,ada,ada,employee,\nBroken Row,onlytwo\nAlan Turing,alan,alan,employee,\n")

	require.Len(t, rows, 3)
	require.Equal(t, mapping.ActionReject, rows[1].Action)
	require.Contains(t, rows[1].Reason, "expected 5 columns")
	require.Equal(t, "Alan Turing", rows[2].Entry.Name)
}

func TestBuildPlanActions(t *testing.T) {
	t.Parallel()

	existing := []mapping.Entry{
		{Name: "Ada Lovelace", GitHub: "ada", K8s: "ada", Class: mapping.ClassEmployee},
		{Name: "Alan Turing", GitHub: "alan", K8s: "alan", Class: mapping.ClassEmployee},
	}

	rows := parse(t,
		"Ada Lovelace,ada,ada,employee,\n"+ // unchanged
			"Alan Turing,turing,alan,employee,\n"+ // update: handle changes
			"Grace Hopper,grace,grace,employee,\n") // create

	plan := mapping.BuildPlan(rows, existing)

	require.Equal(t, mapping.ActionUnchanged, plan.Rows[0].Action)
	require.Equal(t, mapping.ActionUpdate, plan.Rows[1].Action)
	require.Contains(t, plan.Rows[1].Reason, "github: alan → turing")
	require.NotNil(t, plan.Rows[1].Before)
	require.Equal(t, mapping.ActionCreate, plan.Rows[2].Action)

	creates, updates, unchanged, rejects := plan.Counts()
	require.Equal(t, 1, creates)
	require.Equal(t, 1, updates)
	require.Equal(t, 1, unchanged)
	require.Zero(t, rejects)
	require.True(t, plan.Applicable())
	require.False(t, plan.HasRejects())
}

// A file that maps two people to one handle must be caught before it is
// half-applied.
func TestPlanCatchesCollisionsWithinTheFile(t *testing.T) {
	t.Parallel()

	rows := parse(t, "Ada Lovelace,shared,ada,employee,\nAlan Turing,shared,alan,employee,\n")

	plan := mapping.BuildPlan(rows, nil)

	require.Equal(t, mapping.ActionCreate, plan.Rows[0].Action)
	require.Equal(t, mapping.ActionReject, plan.Rows[1].Action)
	require.Contains(t, plan.Rows[1].Reason, "already taken")
}

func TestPlanCatchesDuplicateNamesWithinTheFile(t *testing.T) {
	t.Parallel()

	rows := parse(t, "Ada Lovelace,ada,ada,employee,\nAda Lovelace,other,otherabbr,employee,\n")

	plan := mapping.BuildPlan(rows, nil)

	require.Equal(t, mapping.ActionReject, plan.Rows[1].Action)
	require.Contains(t, plan.Rows[1].Reason, "also appears on line 2")
}

// The store's invariants apply to an import exactly as to a form: an
// abbreviation cannot be reassigned once a namespace exists under it.
func TestPlanEnforcesImmutableAbbreviation(t *testing.T) {
	t.Parallel()

	existing := []mapping.Entry{{Name: "Ada Lovelace", GitHub: "ada", K8s: "ada", Class: mapping.ClassEmployee}}

	plan := mapping.BuildPlan(parse(t, "Ada Lovelace,ada,renamed,employee,\n"), existing)

	require.Equal(t, mapping.ActionReject, plan.Rows[0].Action)
	require.Contains(t, plan.Rows[0].Reason, "immutable")
}

func TestPlanRejectsInvalidEntries(t *testing.T) {
	t.Parallel()

	plan := mapping.BuildPlan(parse(t,
		"Ada Lovelace,ada,BADABBR,employee,\n"+
			"Bad Class,bc,bc,contractor,\n"+
			"Bad Pin,bp,bp,bot,robots\n"), nil)

	require.Equal(t, mapping.ActionReject, plan.Rows[0].Action)
	require.Contains(t, plan.Rows[0].Reason, "DNS-1123")
	require.Equal(t, mapping.ActionReject, plan.Rows[1].Action)
	require.Contains(t, plan.Rows[1].Reason, "class")
	require.Equal(t, mapping.ActionReject, plan.Rows[2].Action)
	require.Contains(t, plan.Rows[2].Reason, "<org>/<team>")
}

// Re-importing a corrected file should say "nothing to do" rather than
// silently doing nothing.
func TestUnchangedPlanIsNotApplicable(t *testing.T) {
	t.Parallel()

	existing := []mapping.Entry{{Name: "Ada Lovelace", GitHub: "ada", K8s: "ada", Class: mapping.ClassEmployee}}

	plan := mapping.BuildPlan(parse(t, "Ada Lovelace,ada,ada,employee,\n"), existing)

	require.False(t, plan.Applicable())
	require.Equal(t, mapping.ActionUnchanged, plan.Rows[0].Action)
}
