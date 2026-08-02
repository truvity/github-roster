package audit_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/audit/audittest"
	"github.com/truvity/github-roster/pkg/peribolos"
)

func TestMemorySink(t *testing.T) {
	t.Parallel()

	audittest.SinkSuite(t, func(*testing.T) audit.Sink { return audit.NewMemory() })
}

// Keys must sort chronologically, because that is how the list page and the
// S3 sink both find the newest run without an index.
func TestIDsSortChronologically(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	first := audit.NewID(base, "job-a")
	second := audit.NewID(base.Add(time.Second), "job-a")

	require.Less(t, first, second)
}

// An object key must survive whatever an organization or Job is called.
func TestKeysAreSafe(t *testing.T) {
	t.Parallel()

	id := audit.NewID(time.Now(), "Job/With Spaces_And%Junk")
	key := audit.Key("Some Org", id)

	require.NotContains(t, key, " ")
	require.NotContains(t, key, "%")
	require.Equal(t, "some-org", audit.OrgFromKey(key))
	require.Contains(t, key, ".json")
}

func TestFromRunCapturesTheWholeStory(t *testing.T) {
	t.Parallel()

	result := &peribolos.Result{
		Mode:           peribolos.ModeFull,
		Org:            "example-org",
		YAML:           "orgs: {}\n",
		Removing:       []string{"gone"},
		Adding:         []string{"joiner"},
		SkippedSources: []string{"corp"},
		Notes:          []string{"a note"},
	}

	run := &audit.Run{
		JobName:   "roster-sync-1",
		Confirmed: true,
		Succeeded: true,
		StartedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}

	record := audit.FromRun(audit.TriggerOperator, audit.Actor{Subject: "sub-1", Email: "operator@example.com"}, result, run, nil, nil)

	require.Equal(t, "example-org", record.Org)
	require.Equal(t, peribolos.ModeFull, record.Mode)
	require.True(t, record.Confirmed)
	require.Equal(t, []string{"joiner"}, record.Adding)
	require.Equal(t, []string{"gone"}, record.Removing)
	require.Equal(t, "orgs: {}\n", record.Config)
	require.Equal(t, []string{"corp"}, record.SkippedSources)
	require.Equal(t, []string{"a note"}, record.Notes)
	require.Equal(t, run, record.Job)
	require.Empty(t, record.Error)
}

// A failed run is still recorded: the attempt is the thing being audited.
func TestFailedRunsAreRecorded(t *testing.T) {
	t.Parallel()

	record := audit.FromRun(audit.TriggerSchedule, audit.Actor{}, nil, nil, nil,
		errRun("the reconciler could not start"))

	require.Equal(t, audit.TriggerSchedule, record.Trigger)
	require.Contains(t, record.Error, "could not start")
	require.NotEmpty(t, record.ID, "even a run that produced no Job needs an identifier")
}

func TestSummary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		record audit.Record
		want   string
	}{
		{audit.Record{Confirmed: true, Adding: []string{"a"}, Removing: []string{"b", "c"}}, "applied — 1 added, 2 removed"},
		{audit.Record{Confirmed: false}, "dry run — no changes"},
		{audit.Record{Confirmed: true, Error: "boom"}, "applied — failed"},
	}

	for _, tc := range cases {
		require.Equal(t, tc.want, tc.record.Summary())
	}
}

type errRunString string

func (e errRunString) Error() string { return string(e) }

func errRun(s string) error { return errRunString(s) }
