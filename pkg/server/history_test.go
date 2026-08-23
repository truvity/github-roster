package server

import (
	"testing"
	"time"

	"github.com/truvity/github-roster/pkg/audit"
)

func TestFlattenChanges(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	records := []audit.Record{
		{Org: "acme", At: now, Confirmed: true, Kind: audit.KindReconcile, Actor: "reconciler", Removing: []string{"lhaas"}},
		{Org: "acme", At: now.Add(-time.Hour), Confirmed: true, Kind: audit.KindOperatorSync, ActorEmail: "o@x", Adding: []string{"mstrand"}},
		{Org: "beta", At: now.Add(-2 * time.Hour), Confirmed: false, Kind: audit.KindOperatorSync, Adding: []string{"ignored-preview"}},
	}

	// A record with per-change detail is preferred over Adding/Removing.
	rich := []audit.Record{{Org: "acme", At: now, Confirmed: true, Kind: audit.KindReconcile,
		Adding:  []string{"ignored-when-changes-present"},
		Changes: []audit.Change{{Verb: "team-added", Subject: "dana", Team: "team-eng"}}}}
	rc := flattenChanges(rich, "", "")
	if len(rc) != 1 || rc[0].Verb != "team-added" || rc[0].Subject != "dana" {
		t.Fatalf("Changes should be preferred: %+v", rc)
	}

	all := flattenChanges(records, "", "")
	if len(all) != 2 {
		t.Fatalf("expected 2 changes (preview excluded), got %d: %+v", len(all), all)
	}
	// Newest first: the reconcile removal precedes the earlier sync add.
	if all[0].Verb != "removed" || all[0].Subject != "lhaas" || all[0].Kind != audit.KindReconcile {
		t.Fatalf("unexpected first row: %+v", all[0])
	}

	// Kind filter.
	onlySync := flattenChanges(records, string(audit.KindOperatorSync), "")
	if len(onlySync) != 1 || onlySync[0].Subject != "mstrand" {
		t.Fatalf("kind filter failed: %+v", onlySync)
	}

	// Person filter (by login).
	byPerson := flattenChanges(records, "", "lhaas")
	if len(byPerson) != 1 || byPerson[0].Subject != "lhaas" {
		t.Fatalf("person filter failed: %+v", byPerson)
	}
}
