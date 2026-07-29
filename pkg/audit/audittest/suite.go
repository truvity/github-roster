// Package audittest holds the contract every [audit.Sink] must satisfy.
//
// An ordinary package rather than a _test file, so the integration suite
// runs the same assertions against a real bucket that the unit suite runs
// against memory. A fake that behaves differently from the real thing is
// worse than no fake at all.
package audittest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/peribolos"
)

func record(org string, at time.Time, confirmed bool) audit.Record {
	return audit.Record{
		ID:        audit.NewID(at, "job-"+org),
		At:        at,
		Org:       org,
		Trigger:   audit.TriggerOperator,
		Actor:     "operator@example.com",
		Mode:      peribolos.ModeFull,
		Confirmed: confirmed,
		Removing:  []string{"gone"},
		Config:    "orgs:\n  " + org + ":\n    members: []\n",
	}
}

// SinkSuite runs the sink contract against a factory.
func SinkSuite(t *testing.T, newSink func(t *testing.T) audit.Sink) {
	t.Helper()

	ctx := context.Background()
	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	t.Run("empty sink lists nothing", func(t *testing.T) {
		records, err := newSink(t).List(ctx, "", 0)
		require.NoError(t, err)
		require.Empty(t, records)
	})

	t.Run("write then read back in full", func(t *testing.T) {
		sink := newSink(t)
		want := record("example-org", base, true)

		require.NoError(t, sink.Write(ctx, want))

		records, err := sink.List(ctx, "example-org", 0)
		require.NoError(t, err)
		require.Len(t, records, 1)

		got := records[0]
		require.Equal(t, want.ID, got.ID)
		require.Equal(t, want.Org, got.Org)
		require.Equal(t, want.Actor, got.Actor)
		require.Equal(t, want.Trigger, got.Trigger)
		require.True(t, got.Confirmed)
		require.Equal(t, want.Removing, got.Removing)
		// The rendered document is stored in full: the diff says what
		// changed, this says on what basis.
		require.Equal(t, want.Config, got.Config)
		require.WithinDuration(t, want.At, got.At, time.Second)
	})

	t.Run("newest first", func(t *testing.T) {
		sink := newSink(t)

		for i := range 3 {
			require.NoError(t, sink.Write(ctx, record("example-org", base.Add(time.Duration(i)*time.Hour), true)))
		}

		records, err := sink.List(ctx, "example-org", 0)
		require.NoError(t, err)
		require.Len(t, records, 3)

		for i := 1; i < len(records); i++ {
			require.True(t, records[i-1].At.After(records[i].At),
				"records must be newest first, got %s before %s", records[i-1].At, records[i].At)
		}
	})

	t.Run("filtered by organization", func(t *testing.T) {
		sink := newSink(t)

		require.NoError(t, sink.Write(ctx, record("example-org", base, true)))
		require.NoError(t, sink.Write(ctx, record("other-org", base.Add(time.Hour), true)))

		records, err := sink.List(ctx, "example-org", 0)
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.Equal(t, "example-org", records[0].Org)

		all, err := sink.List(ctx, "", 0)
		require.NoError(t, err)
		require.Len(t, all, 2)
	})

	t.Run("limit is honored", func(t *testing.T) {
		sink := newSink(t)

		for i := range 5 {
			require.NoError(t, sink.Write(ctx, record("example-org", base.Add(time.Duration(i)*time.Hour), true)))
		}

		records, err := sink.List(ctx, "example-org", 2)
		require.NoError(t, err)
		require.Len(t, records, 2)
	})

	// A preview an operator looked at and did NOT apply is part of the
	// story — it is the evidence a change was considered and declined.
	t.Run("dry runs are recorded too", func(t *testing.T) {
		sink := newSink(t)

		require.NoError(t, sink.Write(ctx, record("example-org", base, false)))

		records, err := sink.List(ctx, "example-org", 0)
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.False(t, records[0].Confirmed)
	})
}
