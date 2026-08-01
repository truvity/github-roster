// Package reconciler applies a rendered membership document to a live
// GitHub organization. It is the code that runs INSIDE the applier Job.
//
// It replaced upstream peribolos, which could not express this service's
// ownership split: current peribolos couples --fix-team-members to
// --fix-teams, and --fix-teams makes the config authoritative for team
// existence and settings — which belong to the structure engine, not the
// roster. This package has the split as a type-level property instead:
// there is no code here that can create, delete, or edit a team, only code
// that changes who is in one.
//
// The scope is exactly:
//
//   - organization membership: invite, role, removal, invitation cancel
//   - membership of the teams NAMED IN THE DOCUMENT — teams the document
//     does not mention do not appear in the plan at all
//
// Everything else about the organization is out of reach by construction.
package reconciler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
)

// Options are one run's guards.
type Options struct {
	// Mode is what the document claims to be. Re-checked here against what
	// the plan actually does, because this is the process holding the
	// write credential and it should not take the renderer's word for it.
	Mode peribolos.Mode
	// MinAdmins refuses a document with fewer owners. A document that
	// omitted the owners would propose removing them; this is the guard
	// that makes that a refusal instead of an accident.
	MinAdmins int
	// MaxRemovalFraction refuses a plan removing more than this share of
	// the organization's current members. The circuit breaker against a
	// directory returning nonsense convincingly.
	MaxRemovalFraction float64
}

// ActionKind is one kind of change.
type ActionKind string

// The complete set of changes this package can make. Team creation,
// deletion, and settings are deliberately not on this list.
const (
	// ActionAddMember invites or adds a login as an ordinary member.
	ActionAddMember ActionKind = "add-member"
	// ActionAddAdmin invites or adds a login as an owner.
	ActionAddAdmin ActionKind = "add-admin"
	// ActionSetRole moves an existing member between member and owner.
	ActionSetRole ActionKind = "set-role"
	// ActionRemoveMember removes a login from the organization.
	ActionRemoveMember ActionKind = "remove-member"
	// ActionCancelInvite withdraws a pending invitation.
	ActionCancelInvite ActionKind = "cancel-invite"
	// ActionTeamAdd adds a login to a team named in the document.
	ActionTeamAdd ActionKind = "team-add"
	// ActionTeamRemove removes a login from a team named in the document.
	ActionTeamRemove ActionKind = "team-remove"
)

// Action is one change.
type Action struct {
	Kind  ActionKind
	Login string
	// Team is set for team actions only.
	Team string
	// Admin is set for org-membership adds and role changes.
	Admin bool
	// InvitationID identifies the invitation a cancel withdraws.
	InvitationID int64
}

// Plan is everything one run would change, plus the notes explaining what
// it deliberately left alone.
type Plan struct {
	Org     string
	Actions []Action
	Notes   []string
}

// BuildPlan diffs the document's organization against its live state.
//
// Pure: no I/O, fully deterministic output order. This is the function the
// tests interrogate, because it is the one that decides who keeps access.
func BuildPlan(doc *peribolos.Document, org string, state *orgstate.State, opts Options) (*Plan, error) {
	cfg, ok := doc.Orgs[org]
	if !ok {
		return nil, fmt.Errorf("document does not describe organization %q", org)
	}

	if len(cfg.Admins) < opts.MinAdmins {
		return nil, fmt.Errorf("document names %d owners for %q, below the minimum of %d",
			len(cfg.Admins), org, opts.MinAdmins)
	}

	plan := &Plan{Org: org}

	memberCount := planMembership(plan, cfg, state)
	if err := planTeams(plan, cfg, state); err != nil {
		return nil, err
	}

	sortActions(plan.Actions)

	if err := checkInvariants(plan, memberCount, opts); err != nil {
		return nil, err
	}

	return plan, nil
}

// desiredMember is one login the document wants in the organization, with
// its case preserved for the report.
type desiredMember struct {
	Login string
	Admin bool
}

// desiredRoles maps lowercase login to the desired membership for everyone
// the document wants in the organization.
func desiredRoles(cfg peribolos.Org) map[string]desiredMember {
	desired := make(map[string]desiredMember, len(cfg.Admins)+len(cfg.Members))

	for _, login := range cfg.Members {
		desired[strings.ToLower(login)] = desiredMember{Login: login}
	}

	// Second, so a login accidentally listed in both is an owner rather
	// than a demotion.
	for _, login := range cfg.Admins {
		desired[strings.ToLower(login)] = desiredMember{Login: login, Admin: true}
	}

	return desired
}

// planMembership fills the plan's organization-level actions and returns
// the current member count, which the removal guard is measured against.
func planMembership(plan *Plan, cfg peribolos.Org, state *orgstate.State) int {
	desired := desiredRoles(cfg)

	current := make(map[string]orgstate.Member, len(state.Members))
	for _, member := range state.Members {
		current[strings.ToLower(member.Login)] = member
	}

	invited := make(map[string]orgstate.Invitation)

	for _, invitation := range state.Invitations {
		if invitation.Login == "" {
			// An email-only invitation has no login to diff against.
			plan.Notes = append(plan.Notes,
				fmt.Sprintf("invitation to %s has no login yet; left alone", invitation.Email))

			continue
		}

		invited[strings.ToLower(invitation.Login)] = invitation
	}

	for login, want := range desired {
		member, isMember := current[login]

		switch {
		case isMember && (member.Role == orgstate.RoleAdmin) != want.Admin:
			plan.Actions = append(plan.Actions, Action{Kind: ActionSetRole, Login: member.Login, Admin: want.Admin})
		case isMember:
			// Present with the right role: nothing to do.
		case invited[login].Login != "":
			plan.Notes = append(plan.Notes,
				fmt.Sprintf("%s has a pending invitation; left for them to accept", invited[login].Login))
		case want.Admin:
			plan.Actions = append(plan.Actions, Action{Kind: ActionAddAdmin, Login: want.Login, Admin: true})
		default:
			plan.Actions = append(plan.Actions, Action{Kind: ActionAddMember, Login: want.Login})
		}
	}

	for login, member := range current {
		if _, wanted := desired[login]; !wanted {
			plan.Actions = append(plan.Actions, Action{Kind: ActionRemoveMember, Login: member.Login})
		}
	}

	for login, invitation := range invited {
		if _, wanted := desired[login]; !wanted {
			plan.Actions = append(plan.Actions, Action{
				Kind:         ActionCancelInvite,
				Login:        invitation.Login,
				InvitationID: invitation.ID,
			})
		}
	}

	return len(current)
}

// planTeams fills the team actions for the teams the document names.
//
// A named team that does not exist is an error, not a creation: team
// existence is the structure engine's, and a team missing between render
// and apply means the world changed under the run — the safe response is
// to stop and be looked at.
func planTeams(plan *Plan, cfg peribolos.Org, state *orgstate.State) error {
	slugs := make([]string, 0, len(cfg.Teams))
	for slug := range cfg.Teams {
		slugs = append(slugs, slug)
	}

	sort.Strings(slugs)

	for _, slug := range slugs {
		liveMembers, exists := state.TeamMembers[slug]
		if !exists {
			return fmt.Errorf("document names team %q which does not exist in %q: "+
				"team existence belongs to the structure engine, refusing to proceed", slug, plan.Org)
		}

		team := cfg.Teams[slug]

		desired := make(map[string]string, len(team.Members)+len(team.Maintainers))
		for _, login := range team.Members {
			desired[strings.ToLower(login)] = login
		}

		for _, login := range team.Maintainers {
			desired[strings.ToLower(login)] = login
		}

		// A team member listing includes its maintainers. Someone present
		// and desired gets NO action — never a membership PUT that could
		// demote a maintainer to a plain member.
		live := make(map[string]string, len(liveMembers))
		for _, login := range liveMembers {
			live[strings.ToLower(login)] = login
		}

		for login, exact := range desired {
			if _, present := live[login]; !present {
				plan.Actions = append(plan.Actions, Action{Kind: ActionTeamAdd, Team: slug, Login: exact})
			}
		}

		for login, exact := range live {
			if _, wanted := desired[login]; !wanted {
				plan.Actions = append(plan.Actions, Action{Kind: ActionTeamRemove, Team: slug, Login: exact})
			}
		}
	}

	return nil
}

// checkInvariants is the last gate before the plan is trusted.
func checkInvariants(plan *Plan, memberCount int, opts Options) error {
	var adds, removals int

	for _, action := range plan.Actions {
		switch action.Kind {
		case ActionAddMember, ActionAddAdmin, ActionTeamAdd:
			adds++
		case ActionRemoveMember:
			removals++
		case ActionSetRole, ActionCancelInvite, ActionTeamRemove:
		}
	}

	// The renderer asserts this too, but this process holds the write
	// credential and re-checks rather than trusts.
	if opts.Mode == peribolos.ModeRemovalsOnly && adds > 0 {
		return fmt.Errorf("removals-only run computed %d additions; refusing", adds)
	}

	if opts.MaxRemovalFraction > 0 && memberCount > 0 {
		fraction := float64(removals) / float64(memberCount)
		if fraction > opts.MaxRemovalFraction {
			return fmt.Errorf("plan removes %d of %d members (%.0f%%), above the %.0f%% circuit breaker",
				removals, memberCount, fraction*100, opts.MaxRemovalFraction*100)
		}
	}

	return nil
}

// sortActions orders a plan deterministically: org-level before teams,
// then by team, then by login. A plan that prints in a stable order can be
// diffed between runs, and the audit records depend on that.
func sortActions(actions []Action) {
	rank := map[ActionKind]int{
		ActionSetRole:      0,
		ActionAddAdmin:     1,
		ActionAddMember:    2,
		ActionCancelInvite: 3,
		ActionRemoveMember: 4,
		ActionTeamAdd:      5,
		ActionTeamRemove:   6,
	}

	sort.Slice(actions, func(i, j int) bool {
		a, b := actions[i], actions[j]

		if a.Team != b.Team {
			return a.Team < b.Team
		}

		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}

		return strings.ToLower(a.Login) < strings.ToLower(b.Login)
	})
}

// Writer is what Execute needs from GitHub. Small on purpose: this
// interface IS the applier's write surface, and everything absent from it
// is a write this service cannot perform.
type Writer interface {
	// SetMembership adds or invites a login with the given role, or
	// changes the role of an existing member.
	SetMembership(ctx context.Context, org, login string, admin bool) error
	// RemoveMember removes a login from the organization.
	RemoveMember(ctx context.Context, org, login string) error
	// CancelInvite withdraws a pending invitation.
	CancelInvite(ctx context.Context, org string, invitationID int64) error
	// AddTeamMember adds a login to an existing team.
	AddTeamMember(ctx context.Context, org, slug, login string) error
	// RemoveTeamMember removes a login from a team.
	RemoveTeamMember(ctx context.Context, org, slug, login string) error
}

// Execute performs the plan serially, in order, stopping at the first
// error. Serially because GitHub's guidance is to serialize writes, and
// stopping because a half-applied plan must be looked at, not pushed
// through — the Job's backoffLimit of zero is the other half of that
// decision.
//
// Each performed action is reported through report, so the Job log is a
// complete account of what actually changed even if a later action fails.
func Execute(ctx context.Context, writer Writer, plan *Plan, report func(string)) error {
	for _, action := range plan.Actions {
		if err := execute(ctx, writer, plan.Org, action); err != nil {
			return fmt.Errorf("%s %s: %w", action.Kind, action.Login, err)
		}

		report(Describe(action, true))
	}

	return nil
}

func execute(ctx context.Context, writer Writer, org string, action Action) error {
	switch action.Kind {
	case ActionAddMember, ActionAddAdmin, ActionSetRole:
		return writer.SetMembership(ctx, org, action.Login, action.Admin)
	case ActionRemoveMember:
		return writer.RemoveMember(ctx, org, action.Login)
	case ActionCancelInvite:
		return writer.CancelInvite(ctx, org, action.InvitationID)
	case ActionTeamAdd:
		return writer.AddTeamMember(ctx, org, action.Team, action.Login)
	case ActionTeamRemove:
		return writer.RemoveTeamMember(ctx, org, action.Team, action.Login)
	default:
		return fmt.Errorf("unknown action kind %q", action.Kind)
	}
}

// Describe renders one action for the run report. done selects between
// "would" (a preview) and past tense (it happened).
func Describe(action Action, done bool) string {
	verb := func(would, past string) string {
		if done {
			return past
		}

		return "would " + would
	}

	switch action.Kind {
	case ActionAddAdmin:
		return fmt.Sprintf("%s %s as owner", verb("add", "added"), action.Login)
	case ActionAddMember:
		return fmt.Sprintf("%s %s as member", verb("add", "added"), action.Login)
	case ActionSetRole:
		role := "member"
		if action.Admin {
			role = "owner"
		}

		return fmt.Sprintf("%s %s to %s", verb("change role of", "changed role of"), action.Login, role)
	case ActionRemoveMember:
		return fmt.Sprintf("%s %s from the organization", verb("remove", "removed"), action.Login)
	case ActionCancelInvite:
		return fmt.Sprintf("%s invitation for %s", verb("cancel", "canceled"), action.Login)
	case ActionTeamAdd:
		return fmt.Sprintf("%s %s to team %s", verb("add", "added"), action.Login, action.Team)
	case ActionTeamRemove:
		return fmt.Sprintf("%s %s from team %s", verb("remove", "removed"), action.Login, action.Team)
	default:
		return fmt.Sprintf("unknown action %q for %s", action.Kind, action.Login)
	}
}

// Report renders the whole plan as the Job's output: what would change or
// did change, and what was deliberately left alone. This text is what the
// audit record and the operator's preview show.
func Report(plan *Plan, done bool) string {
	var out strings.Builder

	fmt.Fprintf(&out, "organization %s: %d change(s)\n", plan.Org, len(plan.Actions))

	for _, action := range plan.Actions {
		fmt.Fprintf(&out, "  %s\n", Describe(action, done))
	}

	for _, note := range plan.Notes {
		fmt.Fprintf(&out, "  note: %s\n", note)
	}

	if len(plan.Actions) == 0 {
		out.WriteString("  nothing to change\n")
	}

	return out.String()
}
