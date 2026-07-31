// Package roster is the join: directory liveness ⋈ the mapping ⋈ GitHub
// state, keyed on a person's name.
//
// Everything the service decides rests on this one computation, so its
// failure behavior is the design, not an afterthought. The rule throughout
// is that missing information produces *inaction*, never action: a person
// nobody can identify is left alone, a directory that did not answer
// removes nobody, and every such case is surfaced rather than swallowed.
package roster

import (
	"sort"
	"strings"
	"time"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
)

// Roster is the joined view.
//
// This type is a published contract: the gitops puller (INF-443) commits it
// and cfggen builds developer namespaces from it. Fields may be added;
// removing or repurposing one breaks a consumer in another repository.
type Roster struct {
	// GeneratedAt is when the join ran.
	GeneratedAt time.Time `json:"generatedAt"`
	// Sources reports each directory's health at join time, so a consumer
	// can refuse to act on a roster built from stale liveness.
	Sources []directory.Status `json:"sources"`
	// People are everyone the join could identify, sorted by name.
	People []Person `json:"people"`
	// Warnings are the things an operator should look at. They never stop
	// the join — a roster that refuses to render because one person is
	// unmapped would be useless precisely when it is needed.
	Warnings []Warning `json:"warnings,omitempty"`
}

// Person is one joined identity.
type Person struct {
	// Name is the join key: "First Last".
	Name string `json:"name"`
	// GitHub is the mapped login. Never empty in this list — a person
	// without one is a warning, not a Person.
	GitHub string `json:"github"`
	// K8s is the namespace abbreviation, empty when none is assigned yet.
	K8s string `json:"k8s,omitempty"`
	// Class is employee or bot.
	Class mapping.Class `json:"class"`
	// Live is whether this person still has an account somewhere.
	Live bool `json:"live"`
	// Sources names the directories that know this person. Empty for a
	// bot, which has no directory account by definition.
	Sources []string `json:"sources,omitempty"`
	// Email is the primary address from the first directory that knew
	// them, for display only. The join never keys on it.
	Email string `json:"email,omitempty"`
	// Directories is the per-source view, keyed by source name: the
	// address each directory knows this person under and whether that
	// account is live there. The identity trace an operator follows —
	// IdP(s) → mapping → org(s) — starts here.
	Directories map[string]DirectoryIdentity `json:"directories,omitempty"`
	// Orgs is this person's GitHub standing, keyed by organization.
	Orgs map[string]Membership `json:"orgs"`
}

// DirectoryIdentity is one directory's view of a person.
type DirectoryIdentity struct {
	// Email is the address this directory knows them under.
	Email string `json:"email"`
	// Live is whether that account is active in this directory.
	Live bool `json:"live"`
}

// Membership is a person's standing in one organization.
type Membership struct {
	// Member is true when GitHub reports them a member OR they have a
	// pending invitation. An invited person occupies a seat and must not
	// be invited twice.
	Member bool `json:"member"`
	// InvitationPending distinguishes the two, because they need
	// different actions from an operator.
	InvitationPending bool `json:"invitationPending,omitempty"`
	// Role is admin or member, empty when not a member.
	Role orgstate.Role `json:"role,omitempty"`
	// Teams they currently belong to.
	Teams []string `json:"teams,omitempty"`
	// DesiredTeams is what the configuration says they should belong to.
	// The difference between this and Teams is what an operator sync would
	// change.
	DesiredTeams []string `json:"desiredTeams,omitempty"`
}

// WarningKind classifies something an operator should look at.
type WarningKind string

const (
	// WarnUnmapped is a live directory person with no mapping entry. They
	// get nothing until an operator maps them — the fail-safe direction.
	WarnUnmapped WarningKind = "unmapped"
	// WarnOrphanedMapping is a mapping entry no directory knows. Inert: it
	// grants nothing, and it is usually a leaver whose entry outlived them
	// or a typo in the name.
	WarnOrphanedMapping WarningKind = "orphaned-mapping"
	// WarnUnknownMember is a GitHub member nobody can identify — not in
	// the mapping and not an exception. These are what an adoption cleanup
	// is made of.
	WarnUnknownMember WarningKind = "unknown-member"
	// WarnNotLiveOwner is an organization owner who is no longer live.
	// Owners are out of this service's hands by design, so this cannot be
	// fixed by a sync — it needs a reviewed infrastructure change, and
	// saying so is the only way it gets done.
	WarnNotLiveOwner WarningKind = "not-live-owner"
	// WarnStaleSource is a directory whose last fetch failed. Its people
	// are still shown, from the last known good read, but its removals
	// must be skipped.
	WarnStaleSource WarningKind = "stale-source"
)

// Warning is one thing worth an operator's attention.
type Warning struct {
	Kind WarningKind `json:"kind"`
	// Subject is the person, login or source the warning is about.
	Subject string `json:"subject"`
	// Org is set when the warning concerns one organization.
	Org string `json:"org,omitempty"`
	// Detail is a human-readable explanation.
	Detail string `json:"detail"`
}

// Inputs are everything the join reads.
type Inputs struct {
	Config *config.Config
	// Snapshots is one per healthy-or-stale source; a source that has
	// never been read successfully is simply absent.
	Snapshots []*directory.Snapshot
	// SourceStatuses reports every configured source, including ones with
	// no snapshot at all.
	SourceStatuses []directory.Status
	// Entries is the whole mapping.
	Entries []mapping.Entry
	// Orgs is the read GitHub state, keyed by organization name.
	Orgs map[string]*orgstate.State
	// Now is injected so tests and golden files are deterministic.
	Now time.Time
}

// Join computes the roster.
func Join(in Inputs) *Roster {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	r := &Roster{
		GeneratedAt: now,
		Sources:     in.SourceStatuses,
		People:      make([]Person, 0, len(in.Entries)),
	}

	for _, status := range in.SourceStatuses {
		if !status.Healthy {
			r.Warnings = append(r.Warnings, Warning{
				Kind:    WarnStaleSource,
				Subject: status.Source,
				Detail:  staleDetail(status),
			})
		}
	}

	live, liveByEmail := livenessIndexes(in.Snapshots)

	// covered marks directory names some entry accounts for — by its own
	// name or through an email match — so a directory spelling a name
	// differently does not double-report the person as unmapped.
	covered := make(map[string]bool, len(in.Entries))

	for i := range in.Entries {
		person, claimed := join(in, in.Entries[i], live, liveByEmail)
		r.People = append(r.People, person)
		covered[in.Entries[i].Name] = true

		for _, name := range claimed {
			covered[name] = true
		}
	}

	r.Warnings = append(r.Warnings, unmappedWarnings(live, covered)...)
	r.Warnings = append(r.Warnings, membershipWarnings(in, r.People)...)

	sort.Slice(r.People, func(i, j int) bool { return r.People[i].Name < r.People[j].Name })
	sortWarnings(r.Warnings)

	return r
}

// liveness is what the directories say about one person.
type liveness struct {
	// name is the display name the directories know this person under.
	name    string
	live    bool
	email   string
	sources []string
	// directories is the per-source view: which address each directory
	// knows this person under, and whether that account is live there.
	directories map[string]DirectoryIdentity
	// groups are the group addresses this person belongs to, lowercased,
	// across every source that knows them.
	groups map[string]bool
}

// livenessIndexes folds every snapshot into one view per person, indexed
// both by display name and by every address the directories know.
//
// A person can exist in more than one directory — the design accepts that
// explicitly — and they count as live if ANY directory still has them
// active. Requiring every directory to agree would offboard people the day
// one of their accounts is closed, which is the opposite of what the
// suspension signal means.
func livenessIndexes(snapshots []*directory.Snapshot) (byNameIdx, byEmailIdx map[string]*liveness) {
	byName := map[string]*liveness{}
	byEmail := map[string]*liveness{}

	// Group membership is merged ACROSS snapshots before users are
	// folded: a group may contain members from another company's domain
	// (identity-model.md cross-company rule — e.g. a truvity person in
	// team-steerco@trustform.io). The member's own directory never lists
	// that group, so per-snapshot inversion would silently drop the
	// membership; liveness still comes only from the person's home
	// directory, which is the rule.
	memberOf := groupsByMember(snapshots...)

	for _, snap := range snapshots {
		if snap == nil {
			continue
		}

		for _, user := range snap.Users {
			if user.Name == "" {
				// Nothing to join on. Not a warning: a directory can hold
				// service accounts and shared mailboxes with no name, and
				// they are not people.
				continue
			}

			l, ok := byName[user.Name]
			if !ok {
				l = &liveness{name: user.Name, groups: map[string]bool{}, directories: map[string]DirectoryIdentity{}}
				byName[user.Name] = l
			}

			if user.Email != "" {
				byEmail[strings.ToLower(user.Email)] = l
			}

			l.live = l.live || user.Live
			l.sources = appendUnique(l.sources, snap.Source)
			l.directories[snap.Source] = DirectoryIdentity{Email: user.Email, Live: user.Live}

			if l.email == "" {
				l.email = user.Email
			}

			for group := range memberOf[strings.ToLower(user.Email)] {
				l.groups[group] = true
			}
		}
	}

	return byName, byEmail
}

// groupsByMember inverts every snapshot's group listing into one
// member → groups view, so membership survives crossing company
// boundaries.
func groupsByMember(snapshots ...*directory.Snapshot) map[string]map[string]bool {
	byMember := map[string]map[string]bool{}

	for _, snap := range snapshots {
		if snap == nil {
			continue
		}

		for group, members := range snap.Groups {
			for _, email := range members {
				email = strings.ToLower(email)
				if byMember[email] == nil {
					byMember[email] = map[string]bool{}
				}

				byMember[email][strings.ToLower(group)] = true
			}
		}
	}

	return byMember
}

// join builds one person from their mapping entry and what the directories
// and GitHub say about them.
func join(in Inputs, entry mapping.Entry, live, liveByEmail map[string]*liveness) (person Person, claimed []string) {
	person = Person{
		Name:   entry.Name,
		GitHub: entry.GitHub,
		K8s:    entry.K8s,
		Class:  entry.Class,
		Orgs:   map[string]Membership{},
	}

	var resolved *liveness
	resolved, claimed = resolveLiveness(entry, live, liveByEmail)

	if resolved != nil {
		person.Live = resolved.live
		person.Email = resolved.email
		person.Sources = resolved.sources
		person.Directories = resolved.directories
	}

	// A bot has no directory account and therefore no liveness signal.
	// Treating "not in any directory" as "gone" would delete every bot on
	// the first scheduled run.
	if entry.Class == mapping.ClassBot {
		person.Live = true
	}

	for i := range in.Config.Orgs {
		org := &in.Config.Orgs[i]

		person.Orgs[org.Name] = membership(in, org, entry, resolved, person.Live)
	}

	return person, claimed
}

// resolveLiveness connects an entry to the directory records that prove the
// person exists: every record one of the entry's EMAILS points at, plus the
// name match as a fallback. Email wins on conflict — the address is the
// authoritative IdP anchor, the name is the human label — so a directory
// spelling the name differently cannot detach a person from their liveness.
// The returned names are the directory spellings the entry accounts for.
func resolveLiveness(entry mapping.Entry, live, liveByEmail map[string]*liveness) (merged *liveness, claimed []string) {
	var (
		records []*liveness
		seen    = map[*liveness]bool{}
	)

	add := func(l *liveness) {
		if l == nil || seen[l] {
			return
		}

		seen[l] = true
		records = append(records, l)
		claimed = append(claimed, l.name)
	}

	for _, email := range entry.Emails {
		add(liveByEmail[strings.ToLower(email)])
	}

	add(live[entry.Name])

	switch len(records) {
	case 0:
		return nil, nil
	case 1:
		return records[0], claimed
	}

	// Several records — a person spanning directories under different
	// display names. Merge them; liveness stays OR across directories.
	merged = &liveness{
		name:        entry.Name,
		groups:      map[string]bool{},
		directories: map[string]DirectoryIdentity{},
	}

	for _, l := range records {
		merged.live = merged.live || l.live
		merged.sources = append(merged.sources, l.sources...)

		if merged.email == "" {
			merged.email = l.email
		}

		for src, id := range l.directories {
			merged.directories[src] = id
		}

		for g := range l.groups {
			merged.groups[g] = true
		}
	}

	return merged, claimed
}

func membership(in Inputs, org *config.Org, entry mapping.Entry, l *liveness, isLive bool) Membership {
	m := Membership{DesiredTeams: desiredTeams(org, entry, l, isLive)}

	state := in.Orgs[org.Name]
	if state == nil {
		// No GitHub read for this org — the desired side is still useful,
		// and pretending they are absent would propose inviting everyone.
		return m
	}

	for _, member := range state.Members {
		if strings.EqualFold(member.Login, entry.GitHub) {
			m.Member = true
			m.Role = member.Role
		}
	}

	for _, invite := range state.Invitations {
		if invite.Login != "" && strings.EqualFold(invite.Login, entry.GitHub) {
			m.Member = true
			m.InvitationPending = true
		}
	}

	for team, logins := range state.TeamMembers {
		for _, login := range logins {
			if strings.EqualFold(login, entry.GitHub) {
				m.Teams = appendUnique(m.Teams, team)
			}
		}
	}

	sort.Strings(m.Teams)

	return m
}

// desiredTeams is what the configuration says this person should be in.
//
// A directory-mapped team draws from the union of its groups, and only for
// people who are live: a suspended person is in no team, whatever the
// directory group still says. A pinned team draws from the mapping entry
// alone, because it has no group to read.
func desiredTeams(org *config.Org, entry mapping.Entry, l *liveness, isLive bool) []string {
	var teams []string

	for name, team := range org.Teams {
		switch {
		case team.Pinned:
			if hasPinned(entry, org.Name, name) {
				teams = append(teams, name)
			}
		case !isLive || l == nil:
			// Not live, or no directory knows them: no group membership
			// can apply.
			continue
		default:
			for _, group := range team.Groups {
				if l.groups[strings.ToLower(group)] {
					teams = append(teams, name)

					break
				}
			}
		}
	}

	sort.Strings(teams)

	return teams
}

func hasPinned(entry mapping.Entry, org, team string) bool {
	want := org + "/" + team

	for _, pinned := range entry.Pinned {
		if strings.EqualFold(pinned, want) {
			return true
		}
	}

	return false
}

// unmappedWarnings reports live directory people with no mapping entry.
//
// This is the fail-safe join in action: they get nothing, and an operator
// is told they exist. Silently inventing a GitHub handle from an email
// address is the alternative, and it is how people end up with access to
// the wrong account.
func unmappedWarnings(live map[string]*liveness, covered map[string]bool) []Warning {
	var warnings []Warning

	for name, l := range live {
		if !l.live {
			continue
		}

		if covered[name] {
			continue
		}

		warnings = append(warnings, Warning{
			Kind:    WarnUnmapped,
			Subject: name,
			Detail:  "live in " + strings.Join(l.sources, ", ") + " but has no mapping entry, so nothing is granted",
		})
	}

	return warnings
}

// membershipWarnings reports what the GitHub side shows that the mapping
// cannot explain.
func membershipWarnings(in Inputs, people []Person) []Warning {
	var warnings []Warning

	known := map[string]Person{}

	for i := range people {
		p := &people[i]
		known[strings.ToLower(p.GitHub)] = people[i]

		if p.Class == mapping.ClassBot {
			continue
		}

		if len(p.Sources) == 0 {
			warnings = append(warnings, Warning{
				Kind:    WarnOrphanedMapping,
				Subject: p.Name,
				Detail:  "mapped to " + p.GitHub + " but no directory knows this name; the entry grants nothing",
			})
		}
	}

	for i := range in.Config.Orgs {
		org := &in.Config.Orgs[i]

		state := in.Orgs[org.Name]
		if state == nil {
			continue
		}

		for _, member := range state.Members {
			if org.IsException(member.Login) {
				continue
			}

			person, ok := known[strings.ToLower(member.Login)]
			if !ok {
				warnings = append(warnings, Warning{
					Kind:    WarnUnknownMember,
					Subject: member.Login,
					Org:     org.Name,
					Detail:  "is a member but matches no mapping entry and is not an exception",
				})

				continue
			}

			// An owner who has left cannot be fixed by a sync: owners are
			// registry-pinned by design. Saying so is the only way it gets
			// noticed at all.
			if !person.Live && member.Role == orgstate.RoleAdmin {
				warnings = append(warnings, Warning{
					Kind:    WarnNotLiveOwner,
					Subject: member.Login,
					Org:     org.Name,
					Detail:  person.Name + " is no longer live but is an organization owner; owners change by reviewed infrastructure commit, not by sync",
				})
			}
		}
	}

	return warnings
}

func staleDetail(status directory.Status) string {
	if !status.Ready {
		return "never read successfully; its people are absent from this roster and its removals must be skipped"
	}

	return "last fetch failed (" + status.Error + "); showing the last known good read, and its removals must be skipped"
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}

	return append(list, value)
}

func sortWarnings(warnings []Warning) {
	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].Kind != warnings[j].Kind {
			return warnings[i].Kind < warnings[j].Kind
		}

		return warnings[i].Subject < warnings[j].Subject
	})
}
