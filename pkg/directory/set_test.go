package directory_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/truvity/github-roster/pkg/directory"
)

// stubSource is a no-op Source for Set reconciliation tests.
type stubSource struct{ name string }

func (s stubSource) Name() string { return s.name }
func (s stubSource) Fetch(context.Context) (*directory.Snapshot, error) {
	return &directory.Snapshot{Source: s.name}, nil
}

func names(set *directory.Set) map[string]bool {
	out := map[string]bool{}
	for _, s := range set.Statuses() {
		out[s.Source] = true
	}
	return out
}

func TestSetReconcile(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set := directory.NewSet(log, stubSource{"acme"}, stubSource{"beta"})

	// Warm acme's cache so we can prove Reconcile preserves it.
	set.Refresh(context.Background())
	before := set.Caches()
	var acmeCache = before[0]
	if acmeCache.Name() != "acme" {
		acmeCache = before[1]
	}

	// Reconcile to {acme, gamma}: beta dropped, gamma added, acme kept.
	changed := set.Reconcile([]directory.Source{stubSource{"acme"}, stubSource{"gamma"}})
	if !changed {
		t.Fatal("Reconcile should report a change")
	}

	got := names(set)
	if !got["acme"] || !got["gamma"] || got["beta"] {
		t.Fatalf("after reconcile want {acme,gamma}, got %v", got)
	}

	// acme's cache object is preserved (same pointer), keeping its snapshot.
	after := set.Caches()
	var acmeAfter *directory.Cache
	for _, c := range after {
		if c.Name() == "acme" {
			acmeAfter = c
		}
	}
	if acmeAfter != acmeCache {
		t.Fatal("acme's cache must be preserved across Reconcile")
	}

	// A no-op reconcile reports no change.
	if set.Reconcile([]directory.Source{stubSource{"acme"}, stubSource{"gamma"}}) {
		t.Fatal("identical Reconcile should report no change")
	}
}
