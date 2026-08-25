package reconciler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
)

func document(org peribolos.Org) *peribolos.Document {
	return &peribolos.Document{Orgs: map[string]peribolos.Org{"acme": org}}
}

func kinds(plan *Plan) []string {
	out := make([]string, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		out = append(out, string(action.Kind)+":"+action.Login)
	}

	return out
}

func TestBuildPlanFillsMissingMembersAndTeams(t *testing.T) {
	doc := document(peribolos.Org{
		Admins:  []string{"root-a", "root-b"},
		Members: []string{"alice", "bob"},
		Teams: map[string]peribolos.Team{
			"team-x": {Members: []string{"alice", "bob"}},
		},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
			{Login: "alice", Role: orgstate.RoleMember},
		},
		TeamMembers: map[string][]string{"team-x": {"alice"}},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	want := []string{"add-member:bob", "team-add:bob"}
	if got := kinds(plan); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v, want %v", got, want)
	}
}

func TestBuildPlanNoChurnOnLoginCase(t *testing.T) {
	doc := document(peribolos.Org{
		Admins:  []string{"Root-A", "root-b"},
		Members: []string{"Alice"},
		Teams:   map[string]peribolos.Team{"team-x": {Members: []string{"ALICE"}}},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-B", Role: orgstate.RoleAdmin},
			{Login: "alice", Role: orgstate.RoleMember},
		},
		TeamMembers: map[string][]string{"team-x": {"Alice"}},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if len(plan.Actions) != 0 {
		t.Fatalf("case differences produced churn: %v", kinds(plan))
	}
}

func TestBuildPlanRoleChanges(t *testing.T) {
	doc := document(peribolos.Org{
		Admins:  []string{"root-a", "alice"},
		Members: []string{"root-b"},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
			{Login: "alice", Role: orgstate.RoleMember},
		},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	byLogin := map[string]Action{}
	for _, action := range plan.Actions {
		if action.Kind != ActionSetRole {
			t.Fatalf("unexpected action %v", action)
		}

		byLogin[action.Login] = action
	}

	if action := byLogin["alice"]; !action.Admin {
		t.Errorf("alice should be promoted, got %+v", action)
	}

	if action := byLogin["root-b"]; action.Admin {
		t.Errorf("root-b should be demoted, got %+v", action)
	}
}

func TestBuildPlanLeavesPendingInvitationsAlone(t *testing.T) {
	doc := document(peribolos.Org{
		Admins:  []string{"root-a", "root-b"},
		Members: []string{"invited-one"},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
		},
		Invitations: []orgstate.Invitation{
			{ID: 7, Login: "Invited-One"},
			{Email: "somebody@example.com"},
		},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if len(plan.Actions) != 0 {
		t.Fatalf("pending invitation should be a no-op, got %v", kinds(plan))
	}

	if len(plan.Notes) != 2 {
		t.Fatalf("expected notes for the pending and the email-only invitation, got %v", plan.Notes)
	}
}

func TestBuildPlanCancelsInvitationForRemovedPerson(t *testing.T) {
	doc := document(peribolos.Org{Admins: []string{"root-a", "root-b"}})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
		},
		Invitations: []orgstate.Invitation{{ID: 42, Login: "leaver"}},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionCancelInvite || plan.Actions[0].InvitationID != 42 {
		t.Fatalf("expected one cancel-invite(42), got %+v", plan.Actions)
	}
}

func TestBuildPlanRefusesTooFewAdmins(t *testing.T) {
	doc := document(peribolos.Org{Admins: []string{"root-a"}})

	_, err := BuildPlan(doc, "acme", &orgstate.State{Org: "acme"}, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err == nil || !strings.Contains(err.Error(), "below the minimum") {
		t.Fatalf("expected the min-admins refusal, got %v", err)
	}
}

func TestBuildPlanRefusesUnknownTeam(t *testing.T) {
	doc := document(peribolos.Org{
		Admins: []string{"root-a", "root-b"},
		Teams:  map[string]peribolos.Team{"team-ghost": {Members: []string{"alice"}}},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
		},
		TeamMembers: map[string][]string{},
	}

	_, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err == nil || !strings.Contains(err.Error(), "structure engine") {
		t.Fatalf("expected the unknown-team refusal, got %v", err)
	}
}

func TestBuildPlanAllowsLargeRemoval(t *testing.T) {
	// There is no percentage circuit-breaker anymore: removals follow the
	// directory (the IdP leaver signal), and the safety is upstream — the join
	// holds removals for a source whose read was not authoritative. A plan that
	// legitimately removes a large share must go through.
	doc := document(peribolos.Org{Admins: []string{"root-a", "root-b"}})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
			{Login: "m1", Role: orgstate.RoleMember},
			{Login: "m2", Role: orgstate.RoleMember},
			{Login: "m3", Role: orgstate.RoleMember},
		},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	removals := 0
	for _, a := range plan.Actions {
		if a.Kind == ActionRemoveMember {
			removals++
		}
	}

	if removals != 3 {
		t.Fatalf("expected 3 removals (m1, m2, m3) to be allowed, got %d", removals)
	}
}

func TestBuildPlanRefusesAddsInRemovalsOnlyMode(t *testing.T) {
	doc := document(peribolos.Org{
		Admins:  []string{"root-a", "root-b"},
		Members: []string{"newcomer"},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
		},
	}

	_, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeRemovalsOnly, MinAdmins: 2})
	if err == nil || !strings.Contains(err.Error(), "removals-only") {
		t.Fatalf("expected the removals-only refusal, got %v", err)
	}
}

func TestBuildPlanNeverTouchesUnlistedTeams(t *testing.T) {
	doc := document(peribolos.Org{
		Admins: []string{"root-a", "root-b"},
		Teams:  map[string]peribolos.Team{"team-x": {Members: []string{"root-a"}}},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
		},
		TeamMembers: map[string][]string{
			"team-x":        {"root-a"},
			"engine-owned":  {"root-a", "root-b"},
			"another-thing": {"root-b"},
		},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	for _, action := range plan.Actions {
		if action.Team != "" && action.Team != "team-x" {
			t.Fatalf("plan touched unlisted team %q: %+v", action.Team, action)
		}
	}
}

func TestBuildPlanDoesNotDemoteTeamMaintainers(t *testing.T) {
	doc := document(peribolos.Org{
		Admins:  []string{"root-a", "root-b"},
		Members: []string{"keeper"},
		Teams:   map[string]peribolos.Team{"team-x": {Members: []string{"keeper"}}},
	})

	state := &orgstate.State{
		Org: "acme",
		Members: []orgstate.Member{
			{Login: "root-a", Role: orgstate.RoleAdmin},
			{Login: "root-b", Role: orgstate.RoleAdmin},
			{Login: "keeper", Role: orgstate.RoleMember},
		},
		// keeper might be a maintainer: the listing does not say. The plan
		// must not emit a membership write that could demote them.
		TeamMembers: map[string][]string{"team-x": {"keeper"}},
	}

	plan, err := BuildPlan(doc, "acme", state, Options{Mode: peribolos.ModeFull, MinAdmins: 2})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if len(plan.Actions) != 0 {
		t.Fatalf("expected an empty plan, got %v", kinds(plan))
	}
}

// fakeWriter records calls and can fail on demand.
type fakeWriter struct {
	calls  []string
	failOn string
}

func (f *fakeWriter) record(call string) error {
	f.calls = append(f.calls, call)
	if call == f.failOn {
		return fmt.Errorf("boom")
	}

	return nil
}

func (f *fakeWriter) SetMembership(_ context.Context, org, login string, admin bool) error {
	return f.record(fmt.Sprintf("set:%s:%s:%v", org, login, admin))
}

func (f *fakeWriter) RemoveMember(_ context.Context, org, login string) error {
	return f.record(fmt.Sprintf("remove:%s:%s", org, login))
}

func (f *fakeWriter) CancelInvite(_ context.Context, org string, id int64) error {
	return f.record(fmt.Sprintf("cancel:%s:%d", org, id))
}

func (f *fakeWriter) AddTeamMember(_ context.Context, org, slug, login string) error {
	return f.record(fmt.Sprintf("team-add:%s:%s:%s", org, slug, login))
}

func (f *fakeWriter) RemoveTeamMember(_ context.Context, org, slug, login string) error {
	return f.record(fmt.Sprintf("team-remove:%s:%s:%s", org, slug, login))
}

func TestExecutePerformsActionsInOrder(t *testing.T) {
	plan := &Plan{
		Org: "acme",
		Actions: []Action{
			{Kind: ActionAddMember, Login: "alice"},
			{Kind: ActionTeamAdd, Team: "team-x", Login: "alice"},
		},
	}

	writer := &fakeWriter{}

	var reported []string

	if err := Execute(context.Background(), writer, plan, func(line string) {
		reported = append(reported, line)
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := []string{"set:acme:alice:false", "team-add:acme:team-x:alice"}
	if strings.Join(writer.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", writer.calls, want)
	}

	if len(reported) != 2 {
		t.Fatalf("expected 2 report lines, got %v", reported)
	}
}

func TestExecuteStopsAtFirstError(t *testing.T) {
	plan := &Plan{
		Org: "acme",
		Actions: []Action{
			{Kind: ActionAddMember, Login: "alice"},
			{Kind: ActionAddMember, Login: "bob"},
			{Kind: ActionAddMember, Login: "carol"},
		},
	}

	writer := &fakeWriter{failOn: "set:acme:bob:false"}

	err := Execute(context.Background(), writer, plan, func(string) {})
	if err == nil {
		t.Fatal("expected the second action's error")
	}

	if len(writer.calls) != 2 {
		t.Fatalf("execution should stop at the failure, made calls %v", writer.calls)
	}
}

func TestReportReadsInBothTenses(t *testing.T) {
	plan := &Plan{
		Org: "acme",
		Actions: []Action{
			{Kind: ActionAddMember, Login: "alice"},
			{Kind: ActionRemoveMember, Login: "mallory"},
		},
		Notes: []string{"something was left alone"},
	}

	preview := Report(plan, false)
	for _, want := range []string{"would add alice", "would remove mallory", "left alone"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview)
		}
	}

	done := Report(plan, true)
	if !strings.Contains(done, "added alice") || strings.Contains(done, "would") {
		t.Errorf("done report should be past tense:\n%s", done)
	}
}
