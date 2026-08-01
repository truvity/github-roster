package broker

import (
	"testing"
	"time"

	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/reconciler"
)

func plan(actions ...reconciler.Action) *reconciler.Plan {
	return &reconciler.Plan{Org: "acme", Actions: actions}
}

func TestHashIsContentAddressed(t *testing.T) {
	a := plan(reconciler.Action{Kind: reconciler.ActionTeamAdd, Team: "team-x", Login: "ada"})
	b := plan(reconciler.Action{Kind: reconciler.ActionTeamAdd, Team: "team-x", Login: "ada"})

	if Hash("acme", peribolos.ModeFull, a) != Hash("acme", peribolos.ModeFull, b) {
		t.Fatal("identical plans must hash identically")
	}

	c := plan(reconciler.Action{Kind: reconciler.ActionTeamAdd, Team: "team-x", Login: "bob"})
	if Hash("acme", peribolos.ModeFull, a) == Hash("acme", peribolos.ModeFull, c) {
		t.Fatal("different plans must hash differently")
	}

	// The same actions in another mode or org are a DIFFERENT plan: the
	// hash an operator approves pins all three.
	if Hash("acme", peribolos.ModeFull, a) == Hash("acme", peribolos.ModeRemovalsOnly, a) {
		t.Fatal("mode must be part of the hash")
	}

	if Hash("acme", peribolos.ModeFull, a) == Hash("other", peribolos.ModeFull, a) {
		t.Fatal("org must be part of the hash")
	}
}

func TestPlanStoreExpires(t *testing.T) {
	store := newPlanStore()
	now := time.Now()
	store.now = func() time.Time { return now }

	hash := store.Put(&stored{Org: "acme", Mode: peribolos.ModeFull, Plan: plan()})

	if _, ok := store.Get(hash); !ok {
		t.Fatal("a fresh plan must be retrievable")
	}

	now = now.Add(planTTL + time.Minute)

	if _, ok := store.Get(hash); ok {
		t.Fatal("an expired plan must not be applicable")
	}
}
