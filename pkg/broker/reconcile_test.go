package broker

import (
	"testing"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/config"
)

// ReconcileStatuses returns statuses in configured-org order and omits
// orgs with no recorded status yet.
func TestReconcileStatusesSnapshot(t *testing.T) {
	d := &Deps{Config: &config.Config{Orgs: []config.Org{{Name: "acme"}, {Name: "beta"}}}}

	// Only beta has run so far.
	d.setReconcileStatus(ReconcileStatus{Org: "beta", Actions: 3, Enabled: true})

	got := d.ReconcileStatuses()
	if len(got) != 1 || got[0].Org != "beta" || got[0].Actions != 3 {
		t.Fatalf("expected only beta's status, got %+v", got)
	}

	// Now acme runs too; order follows Config.Orgs (acme, beta).
	d.setReconcileStatus(ReconcileStatus{Org: "acme", Held: true, Reason: "shrink breaker"})

	got = d.ReconcileStatuses()
	if len(got) != 2 || got[0].Org != "acme" || got[1].Org != "beta" {
		t.Fatalf("expected acme,beta in config order, got %+v", got)
	}
	if !got[0].Held || got[0].Reason == "" {
		t.Fatalf("acme should be held with a reason, got %+v", got[0])
	}
}

func TestClassifyKind(t *testing.T) {
	cases := []struct {
		trigger audit.Trigger
		subject string
		want    audit.Kind
	}{
		{audit.TriggerOperator, "u@x", audit.KindOperatorSync},
		{audit.TriggerSchedule, "reconciler", audit.KindReconcile},
		{audit.TriggerSchedule, "schedule", audit.KindRemovals},
	}
	for _, c := range cases {
		if got := classifyKind(c.trigger, audit.Actor{Subject: c.subject}); got != c.want {
			t.Fatalf("classifyKind(%q,%q)=%q want %q", c.trigger, c.subject, got, c.want)
		}
	}
}
