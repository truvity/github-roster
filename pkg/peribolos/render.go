// Package peribolos renders the configuration that the reconciler applies.
//
// This package holds the single most consequential decision in the service:
// **what a run is allowed to change is a property of the rendered document,
// not of a command-line flag.**
//
// An unattended run renders current-GitHub-state minus people known not to
// be live. Nobody who is not already a member can appear in that document,
// so the only change it can produce is a removal — and that is true by
// construction, visible in the YAML, and recorded in the audit trail. A
// flag would be none of those things: it would be invisible in the record,
// one typo away from silently inverting, and impossible to review after
// the fact.
package peribolos

import (
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/roster"
)

// Mode decides what a rendered configuration is capable of.
type Mode string

const (
	// ModeRemovalsOnly is the unattended run. Members are current GitHub
	// state minus the not-live, so only removals are possible, and teams
	// are omitted entirely — organization removal supersedes team
	// membership, so the leaver SLA needs nothing more.
	ModeRemovalsOnly Mode = "removals-only"
	// ModeFull is the operator-triggered sync: the whole desired state,
	// additions included, previewed as a dry run before it is confirmed.
	ModeFull Mode = "full"
)

// Org is one organization's stanza in the reconciler's configuration.
type Org struct {
	// Admins are the organization owners. Rendered from CURRENT state in
	// both modes and never computed: owners are registry-pinned and change
	// by reviewed infrastructure commit. Omitting them would propose
	// removing every owner, which is exactly the accident this service
	// must not have.
	Admins []string `yaml:"admins"`
	// Members are the non-owner members.
	Members []string `yaml:"members"`
	// Teams is omitted entirely in removals-only mode.
	Teams map[string]Team `yaml:"teams,omitempty"`
}

// Team is one team's membership. Creation and deletion are not modeled:
// they belong to the structure engine, and a team present here that does
// not exist on GitHub is skipped rather than created.
type Team struct {
	Maintainers []string `yaml:"maintainers,omitempty"`
	Members     []string `yaml:"members,omitempty"`
}

// Document is the whole rendered configuration.
type Document struct {
	Orgs map[string]Org `yaml:"orgs"`
}

// Result is a render plus what it means, for the preview and the audit
// record.
type Result struct {
	Mode     Mode      `json:"mode"`
	Org      string    `json:"org"`
	Document *Document `json:"-"`
	// YAML is the rendered document, exactly as the Job will receive it.
	YAML string `json:"yaml"`
	// Removing lists the logins this run would remove from the
	// organization.
	Removing []string `json:"removing,omitempty"`
	// Adding lists the logins it would add. Always empty in removals-only
	// mode — that is the invariant, and it is asserted rather than assumed.
	Adding []string `json:"adding,omitempty"`
	// SkippedSources names directories whose data was not trustworthy, and
	// whose people were therefore left alone.
	SkippedSources []string `json:"skippedSources,omitempty"`
	// Notes explain decisions a reader would otherwise have to infer.
	Notes []string `json:"notes,omitempty"`
}

// Inputs are what a render needs.
type Inputs struct {
	Mode Mode
	Org  *config.Org
	// Roster is the join.
	Roster *roster.Roster
	// State is the organization as GitHub reports it now.
	State *orgstate.State
	// UnhealthySources names directories whose last fetch failed. People
	// known only to those directories are left alone.
	UnhealthySources []string
}

// Render produces the configuration for one organization.
func Render(in Inputs) (*Result, error) {
	if in.Org == nil || in.State == nil || in.Roster == nil {
		return nil, fmt.Errorf("render needs a configured org, a roster and current GitHub state")
	}

	result := &Result{Mode: in.Mode, Org: in.Org.Name, SkippedSources: in.UnhealthySources}

	people := peopleByLogin(in.Roster)
	admins, members := splitCurrent(in.State)

	var desired []string

	switch in.Mode {
	case ModeRemovalsOnly:
		desired = removalsOnlyMembers(in, people, members, result)
	case ModeFull:
		desired = fullMembers(in, people, members, result)
	default:
		return nil, fmt.Errorf("unknown render mode %q", in.Mode)
	}

	org := Org{Admins: sortedUnique(admins), Members: sortedUnique(desired)}

	if in.Mode == ModeFull {
		org.Teams = renderTeams(in, result)
	}

	result.Document = &Document{Orgs: map[string]Org{in.Org.Name: org}}

	// The diff is against everyone who OCCUPIES A SEAT, not just confirmed
	// members. An invited person is absent from the members listing but
	// already counted by GitHub, so keeping them in the desired set is not
	// an addition — and computing it the naive way made a removals-only
	// render look like it was granting access.
	occupied := occupiedSeats(in.State)
	result.Removing = missing(occupied, desired)
	result.Adding = missing(desired, occupied)

	// The asymmetry is the whole point, so it is checked rather than
	// trusted. If this ever fires, a rendering change has quietly turned an
	// unattended run into one that can grant access.
	if in.Mode == ModeRemovalsOnly && len(result.Adding) > 0 {
		return nil, fmt.Errorf("removals-only render would ADD %v; refusing to emit it", result.Adding)
	}

	encoded, err := yaml.Marshal(result.Document)
	if err != nil {
		return nil, fmt.Errorf("marshal configuration: %w", err)
	}

	result.YAML = string(encoded)

	return result, nil
}

// removalsOnlyMembers keeps every current member except those positively
// known to be gone.
//
// "Positively known" is doing the work. Absence of evidence is not
// evidence: someone the roster cannot identify at all stays, someone whose
// only directory is unhealthy stays, and someone on the exception list
// stays. The only people who leave are the ones a healthy directory says
// are suspended.
func removalsOnlyMembers(in Inputs, people map[string]roster.Person, current []string, result *Result) []string {
	keep := make([]string, 0, len(current))

	for _, login := range current {
		if in.Org.IsException(login) {
			keep = append(keep, login)

			continue
		}

		person, known := people[strings.ToLower(login)]
		if !known {
			// Not in the mapping. An unattended run must not remove
			// someone nobody can identify — that is an operator's decision,
			// and the join reports them as an unknown member.
			keep = append(keep, login)

			continue
		}

		if person.Live {
			keep = append(keep, login)

			continue
		}

		if skipped := onlyKnownBy(&person, in.UnhealthySources); skipped != "" {
			keep = append(keep, login)
			result.Notes = append(result.Notes,
				fmt.Sprintf("%s (%s) looks not-live, but %s is unhealthy — left alone",
					login, person.Name, skipped))

			continue
		}

		// Genuinely gone.
	}

	// A pending invitation's target must stay in the desired set. Dropping
	// them would cancel an invitation somebody is about to accept, and the
	// person on the other end sees it happen.
	for _, invite := range in.State.Invitations {
		if invite.Login != "" && !containsFold(keep, invite.Login) {
			keep = append(keep, invite.Login)
		}
	}

	return keep
}

// fullMembers renders the desired state: everyone who should be a member.
func fullMembers(in Inputs, people map[string]roster.Person, current []string, result *Result) []string {
	desired := make([]string, 0, len(people))

	// Exceptions are members by decree — Apps, bots, anything whose
	// membership is not a person's.
	for _, login := range current {
		if in.Org.IsException(login) {
			desired = append(desired, login)
		}
	}

	for i := range in.Roster.People {
		person := &in.Roster.People[i]

		membership, ok := person.Orgs[in.Org.Name]
		if !ok {
			continue
		}

		switch {
		case person.Live:
			// A live person belongs if they are already a member or the
			// configuration puts them in a team.
			if membership.Member || len(membership.DesiredTeams) > 0 {
				desired = append(desired, person.GitHub)
			}
		case onlyKnownBy(person, in.UnhealthySources) != "":
			// Not-live according to a directory we do not currently trust.
			if membership.Member {
				desired = append(desired, person.GitHub)
				result.Notes = append(result.Notes,
					fmt.Sprintf("%s (%s) looks not-live, but %s is unhealthy — left alone",
						person.GitHub, person.Name, onlyKnownBy(person, in.UnhealthySources)))
			}
		}
	}

	// Unidentified members are left in place even by an operator sync. The
	// console reports them; removing them is a deliberate act, not a side
	// effect of pressing Sync.
	for _, login := range current {
		if _, known := people[strings.ToLower(login)]; !known && !in.Org.IsException(login) {
			desired = append(desired, login)
			result.Notes = append(result.Notes,
				fmt.Sprintf("%s matches no mapping entry and was left in place; remove them deliberately", login))
		}
	}

	for _, invite := range in.State.Invitations {
		if invite.Login != "" && !containsFold(desired, invite.Login) {
			desired = append(desired, invite.Login)
		}
	}

	return desired
}

// renderTeams renders membership for teams that exist on GitHub.
//
// A configured team that does not exist is skipped, not created: team
// creation belongs to the structure engine, and this service must never be
// the thing that invents one.
func renderTeams(in Inputs, result *Result) map[string]Team {
	live := map[string]bool{}
	for _, team := range in.State.Teams {
		live[team.Slug] = true
	}

	absent := map[string]bool{}
	for _, group := range in.Roster.AbsentGroups {
		absent[group] = true
	}

	teams := map[string]Team{}

	for name := range in.Org.Teams {
		if !live[name] {
			continue
		}

		// A team backed by a group its (healthy) directory says does not
		// exist yet gets a NO-OP: current members restated verbatim. An
		// empty desired list here would read as "remove everyone" the
		// moment the team has members — the per-group fail-safe.
		if group := absentBacking(in.Org.Teams[name], absent); group != "" {
			teams[name] = Team{Members: sortedUnique(in.State.TeamMembers[name])}
			result.Notes = append(result.Notes,
				fmt.Sprintf("team %s: group %s does not exist in its directory yet — membership left untouched", name, group))

			continue
		}

		var members []string

		for i := range in.Roster.People {
			person := &in.Roster.People[i]

			membership, ok := person.Orgs[in.Org.Name]
			if !ok {
				continue
			}

			for _, desired := range membership.DesiredTeams {
				if desired == name {
					members = append(members, person.GitHub)

					break
				}
			}
		}

		// A pinned team keeps its current members no matter what: its
		// residents include non-humans (App and bot logins) that no
		// mapping entry describes, so a computed list can only ever be
		// missing them. Pins may add someone; removal from a pinned team
		// is a manual, deliberate act.
		if in.Org.Teams[name].Pinned {
			current := in.State.TeamMembers[name]
			if extra := missingFrom(members, current); len(extra) > 0 {
				result.Notes = append(result.Notes,
					fmt.Sprintf("team %s is pinned: keeping current member(s) %s", name, strings.Join(extra, ", ")))
			}

			members = append(members, current...)
		}

		teams[name] = Team{Members: sortedUnique(members)}
	}

	if len(teams) == 0 {
		return nil
	}

	return teams
}

// onlyKnownBy reports an unhealthy source this person depends on, or "".
//
// Both the sources that HAVE answered for the person and the ones their
// entry's emails say SHOULD answer count: a person whose only declared
// directory has never produced a snapshot must be protected exactly like
// one whose directory went stale (the 2026-07-31 near-miss).
func onlyKnownBy(person *roster.Person, unhealthy []string) string {
	sources := append(append([]string{}, person.Sources...), person.ExpectedSources...)

	if len(unhealthy) == 0 || len(sources) == 0 {
		return ""
	}

	for _, source := range sources {
		for _, bad := range unhealthy {
			if source == bad {
				return source
			}
		}
	}

	return ""
}

func peopleByLogin(r *roster.Roster) map[string]roster.Person {
	byLogin := make(map[string]roster.Person, len(r.People))
	for i := range r.People {
		byLogin[strings.ToLower(r.People[i].GitHub)] = r.People[i]
	}

	return byLogin
}

// occupiedSeats is every login GitHub already counts in the MEMBERS list:
// confirmed members and outstanding invitations alike.
//
// Owners are excluded because they are rendered in `admins:` and passed
// through untouched — diffing them against `members:` would report every
// owner as a removal the run was about to make.
func occupiedSeats(state *orgstate.State) []string {
	seats := make([]string, 0, len(state.Members)+len(state.Invitations))

	for _, member := range state.Members {
		if member.Role == orgstate.RoleAdmin {
			continue
		}

		seats = append(seats, member.Login)
	}

	for _, invite := range state.Invitations {
		if invite.Login != "" {
			seats = append(seats, invite.Login)
		}
	}

	return seats
}

func splitCurrent(state *orgstate.State) (admins, members []string) {
	for _, member := range state.Members {
		if member.Role == orgstate.RoleAdmin {
			admins = append(admins, member.Login)

			continue
		}

		members = append(members, member.Login)
	}

	return admins, members
}

// missing returns the entries of a absent from b, case-insensitively.
func missing(a, b []string) []string {
	var out []string

	for _, value := range a {
		if !containsFold(b, value) {
			out = append(out, value)
		}
	}

	sort.Strings(out)

	return out
}

func containsFold(list []string, value string) bool {
	for _, existing := range list {
		if strings.EqualFold(existing, value) {
			return true
		}
	}

	return false
}

// missingFrom returns the current members a computed list would drop,
// case-insensitively, in their original case.
func missingFrom(computed, current []string) []string {
	have := map[string]bool{}
	for _, login := range computed {
		have[strings.ToLower(login)] = true
	}

	var missing []string

	for _, login := range current {
		if !have[strings.ToLower(login)] {
			missing = append(missing, login)
		}
	}

	return sortedUnique(missing)
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))

	for _, value := range values {
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, value)
	}

	sort.Strings(out)

	return out
}

// absentBacking returns the first of the team's groups that a healthy
// directory reported absent, or "".
func absentBacking(team config.Team, absent map[string]bool) string {
	for _, group := range team.Groups {
		if absent[strings.ToLower(group)] {
			return group
		}
	}

	return ""
}
