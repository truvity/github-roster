package roster_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/roster"
)

const doc = `
oidc: {disabled: true}
companies:
  corp:
    directory:
      ssmPrefix: /secrets/directory/corp
      domains: [example.com]
    github:
      org: example-org
      consoleAppSSM: /secrets/roster/console/example-org
      applierAppSSM: /secrets/roster/applier/example-org
      exceptions: [example-bot-app]
      teams:
        team-engineers:
          groups: [team-engineers@example.com]
        team-listed:
          members: [alan@example.com, gone@example.com]
        robots:
          pinned: true
`

func cfg(t *testing.T) *config.Config {
	t.Helper()

	parsed, err := config.Parse([]byte(doc))
	require.NoError(t, err)

	return parsed
}

func snapshot() *directory.Snapshot {
	return &directory.Snapshot{
		Source: "corp",
		Users: []directory.User{
			{Name: "Ada Lovelace", Email: "ada@example.com", Live: true},
			{Name: "Alan Turing", Email: "alan@example.com", Live: true},
			{Name: "Gone Person", Email: "gone@example.com", Live: false},
			{Name: "Unmapped Person", Email: "unmapped@example.com", Live: true},
		},
		Groups: map[string][]string{
			"team-engineers@example.com": {"ada@example.com", "gone@example.com", "unmapped@example.com"},
		},
		FetchedAt: time.Now(),
	}
}

func entries() []mapping.Entry {
	return []mapping.Entry{
		{Name: "Ada Lovelace", GitHub: "ada", K8s: "ada", Class: mapping.ClassEmployee},
		{Name: "Alan Turing", GitHub: "alan", K8s: "alan", Class: mapping.ClassEmployee},
		{Name: "Gone Person", GitHub: "gone", K8s: "gone", Class: mapping.ClassEmployee},
		{Name: "Ghost Entry", GitHub: "ghost", Class: mapping.ClassEmployee},
		{Name: "Example Bot", GitHub: "example-bot", Class: mapping.ClassBot, Pinned: []string{"example-org/robots"}},
	}
}

func orgState() *orgstate.State {
	return &orgstate.State{
		Org: "example-org",
		Members: []orgstate.Member{
			{Login: "ada", Role: orgstate.RoleMember},
			{Login: "gone", Role: orgstate.RoleAdmin},
			{Login: "stranger", Role: orgstate.RoleMember},
			{Login: "example-bot-app", Role: orgstate.RoleMember},
		},
		Invitations: []orgstate.Invitation{{Login: "alan", Role: "direct_member"}},
		Teams:       []orgstate.Team{{Slug: "team-engineers"}, {Slug: "robots"}},
		TeamMembers: map[string][]string{"team-engineers": {"ada"}, "robots": {}},
	}
}

func join(t *testing.T) *roster.Roster {
	t.Helper()

	return roster.Join(roster.Inputs{
		Config:         cfg(t),
		Snapshots:      []*directory.Snapshot{snapshot()},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries:        entries(),
		Orgs:           map[string]*orgstate.State{"example-org": orgState()},
		Now:            time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
}

func person(t *testing.T, r *roster.Roster, name string) roster.Person {
	t.Helper()

	for i := range r.People {
		if r.People[i].Name == name {
			return r.People[i]
		}
	}

	t.Fatalf("person %q not in roster", name)

	return roster.Person{}
}

func warnings(r *roster.Roster, kind roster.WarningKind) []roster.Warning {
	var found []roster.Warning

	for _, w := range r.Warnings {
		if w.Kind == kind {
			found = append(found, w)
		}
	}

	return found
}

func TestLiveMappedPerson(t *testing.T) {
	t.Parallel()

	ada := person(t, join(t), "Ada Lovelace")

	require.True(t, ada.Live)
	require.Equal(t, []string{"corp"}, ada.Sources)
	require.Equal(t, "ada@example.com", ada.Email)

	m := ada.Orgs["example-org"]
	require.True(t, m.Member)
	require.False(t, m.InvitationPending)
	require.Equal(t, []string{"team-engineers"}, m.Teams)
	require.Equal(t, []string{"team-engineers"}, m.DesiredTeams)
}

// An invited person occupies a seat and is absent from the members API.
// Counting them as a non-member sends a second invitation; counting them as
// a plain member hides that they have not accepted.
func TestPendingInvitationCountsAsMembership(t *testing.T) {
	t.Parallel()

	m := person(t, join(t), "Alan Turing").Orgs["example-org"]

	require.True(t, m.Member)
	require.True(t, m.InvitationPending)
}

// The fail-safe direction: a live person nobody has mapped gets nothing,
// and an operator is told. Inventing a handle from an email address is how
// people end up with access to the wrong account.
func TestUnmappedLivePersonGetsNothingAndIsReported(t *testing.T) {
	t.Parallel()

	r := join(t)

	for _, p := range r.People {
		require.NotEqual(t, "Unmapped Person", p.Name, "an unmapped person must not appear as a roster entry")
	}

	unmapped := warnings(r, roster.WarnUnmapped)
	require.Len(t, unmapped, 1)
	require.Equal(t, "Unmapped Person", unmapped[0].Subject)
}

// A mapping entry no directory knows grants nothing. Usually a leaver whose
// entry outlived them, or a misspelled name.
func TestOrphanedMappingIsInertAndReported(t *testing.T) {
	t.Parallel()

	r := join(t)

	ghost := person(t, r, "Ghost Entry")
	require.False(t, ghost.Live)
	require.Empty(t, ghost.Sources)
	require.Empty(t, ghost.Orgs["example-org"].DesiredTeams, "an unknown person must be desired in no team")

	orphaned := warnings(r, roster.WarnOrphanedMapping)
	require.Len(t, orphaned, 1)
	require.Equal(t, "Ghost Entry", orphaned[0].Subject)
}

// A suspended person is in no team, whatever the directory group still
// says — group membership outlives suspension routinely.
func TestSuspendedPersonIsDesiredInNoTeam(t *testing.T) {
	t.Parallel()

	gone := person(t, join(t), "Gone Person")

	require.False(t, gone.Live)
	require.Empty(t, gone.Orgs["example-org"].DesiredTeams)
}

// Bots have no directory account, so "not in any directory" must not read
// as "gone" — otherwise the first scheduled run deletes every bot.
func TestBotIsLiveWithoutADirectory(t *testing.T) {
	t.Parallel()

	r := join(t)
	bot := person(t, r, "Example Bot")

	require.True(t, bot.Live)
	require.Empty(t, bot.Sources)
	require.Equal(t, []string{"robots"}, bot.Orgs["example-org"].DesiredTeams)

	for _, w := range warnings(r, roster.WarnOrphanedMapping) {
		require.NotEqual(t, "Example Bot", w.Subject, "a bot must not be reported as an orphaned mapping")
	}
}

func TestUnknownMemberIsReportedAndExceptionsAreNot(t *testing.T) {
	t.Parallel()

	unknown := warnings(join(t), roster.WarnUnknownMember)

	require.Len(t, unknown, 1)
	require.Equal(t, "stranger", unknown[0].Subject)
	require.Equal(t, "example-org", unknown[0].Org)
}

// Owners are registry-pinned by design, so a departed owner cannot be
// fixed by a sync. Reporting it is the only way it gets noticed.
func TestNotLiveOwnerIsReported(t *testing.T) {
	t.Parallel()

	owners := warnings(join(t), roster.WarnNotLiveOwner)

	require.Len(t, owners, 1)
	require.Equal(t, "gone", owners[0].Subject)
	require.Contains(t, owners[0].Detail, "reviewed infrastructure commit")
}

// The most important failure mode in the whole service: a directory that
// did not answer must not look like everybody leaving.
func TestStaleSourceIsReportedAndKeepsItsPeople(t *testing.T) {
	t.Parallel()

	r := roster.Join(roster.Inputs{
		Config:    cfg(t),
		Snapshots: []*directory.Snapshot{snapshot()},
		SourceStatuses: []directory.Status{{
			Source: "corp", Healthy: false, Ready: true, Error: "directory unreachable",
		}},
		Entries: entries(),
		Orgs:    map[string]*orgstate.State{"example-org": orgState()},
	})

	stale := warnings(r, roster.WarnStaleSource)
	require.Len(t, stale, 1)
	require.Contains(t, stale[0].Detail, "removals must be skipped")

	require.True(t, person(t, r, "Ada Lovelace").Live,
		"the last known good read must still count people as live")
}

func TestNeverReadSourceSaysSo(t *testing.T) {
	t.Parallel()

	r := roster.Join(roster.Inputs{
		Config:         cfg(t),
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: false, Ready: false}},
		Entries:        entries(),
	})

	stale := warnings(r, roster.WarnStaleSource)
	require.Len(t, stale, 1)
	require.Contains(t, stale[0].Detail, "never read successfully")
}

// A person can exist in several directories. They are live if ANY of them
// still has them: requiring unanimity would offboard someone the day one of
// their accounts closes.
func TestLiveInAnySourceCounts(t *testing.T) {
	t.Parallel()

	second := &directory.Snapshot{
		Source: "other",
		Users:  []directory.User{{Name: "Gone Person", Email: "gone@other.example", Live: true}},
		Groups: map[string][]string{},
	}

	r := roster.Join(roster.Inputs{
		Config:    cfg(t),
		Snapshots: []*directory.Snapshot{snapshot(), second},
		SourceStatuses: []directory.Status{
			{Source: "corp", Healthy: true, Ready: true},
			{Source: "other", Healthy: true, Ready: true},
		},
		Entries: entries(),
	})

	gone := person(t, r, "Gone Person")
	require.True(t, gone.Live, "suspended in one directory, active in another, is still live")
	require.ElementsMatch(t, []string{"corp", "other"}, gone.Sources)
}

// GitHub logins are case-insensitive; a mapping typed with different
// capitalization than the org reports must still match, or a present member
// looks absent and a scheduled run removes them.
func TestGitHubLoginMatchingIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	state := orgState()
	state.Members = []orgstate.Member{{Login: "ADA", Role: orgstate.RoleMember}}
	state.TeamMembers = map[string][]string{"team-engineers": {"AdA"}}
	state.Invitations = nil

	r := roster.Join(roster.Inputs{
		Config:         cfg(t),
		Snapshots:      []*directory.Snapshot{snapshot()},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries:        entries(),
		Orgs:           map[string]*orgstate.State{"example-org": state},
	})

	m := person(t, r, "Ada Lovelace").Orgs["example-org"]
	require.True(t, m.Member)
	require.Equal(t, []string{"team-engineers"}, m.Teams)
}

// Without a GitHub read the desired side is still computed, but nobody may
// be reported absent — that would propose inviting the entire company.
func TestMissingOrgStateProposesNothing(t *testing.T) {
	t.Parallel()

	r := roster.Join(roster.Inputs{
		Config:         cfg(t),
		Snapshots:      []*directory.Snapshot{snapshot()},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries:        entries(),
		Orgs:           nil,
	})

	ada := person(t, r, "Ada Lovelace").Orgs["example-org"]
	require.False(t, ada.Member)
	require.Equal(t, []string{"team-engineers"}, ada.DesiredTeams)
	require.Empty(t, warnings(r, roster.WarnUnknownMember))
}

func TestPeopleAreSortedForStableOutput(t *testing.T) {
	t.Parallel()

	r := join(t)

	for i := 1; i < len(r.People); i++ {
		require.LessOrEqual(t, r.People[i-1].Name, r.People[i].Name)
	}
}

// The email is the authoritative IdP anchor: an entry whose emails point at
// a directory record wins that record's liveness even when the directory
// spells the name differently — and the person is not double-reported as
// unmapped under the directory's spelling.
func TestJoinMatchesByEmailFirst(t *testing.T) {
	snap := &directory.Snapshot{
		Source: "corp",
		Users: []directory.User{
			{Name: "Lela D", Email: "jelena@example.com", Live: true},
		},
		Groups: map[string][]string{
			"team-engineers@example.com": {"jelena@example.com"},
		},
		FetchedAt: time.Now(),
	}

	r := roster.Join(roster.Inputs{
		Config:         cfg(t),
		Snapshots:      []*directory.Snapshot{snap},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries: []mapping.Entry{{
			Name:   "Jelena Dorotka",
			GitHub: "lela-do",
			Emails: []string{"jelena@example.com"},
			Class:  mapping.ClassEmployee,
		}},
		Orgs: map[string]*orgstate.State{},
	})

	require.Len(t, r.People, 1)
	require.True(t, r.People[0].Live, "email match must carry liveness despite the name mismatch")
	require.Equal(t, "jelena@example.com", r.People[0].Directories["corp"].Email)

	for _, w := range r.Warnings {
		require.NotEqual(t, roster.WarnUnmapped, w.Kind,
			"the directory spelling is claimed via the email; no unmapped warning for %q", w.Subject)
	}
}

// A team declared by explicit member emails behaves exactly like a
// group-backed one: live listed people are desired, and the list never
// resurrects someone their directory says is gone.
func TestExplicitMemberListDesiresOnlyTheLive(t *testing.T) {
	t.Parallel()

	r := join(t)

	alan := person(t, r, "Alan Turing")
	require.Contains(t, alan.Orgs["example-org"].DesiredTeams, "team-listed")

	gone := person(t, r, "Gone Person")
	require.NotContains(t, gone.Orgs["example-org"].DesiredTeams, "team-listed",
		"a static list must not resurrect a suspended person")
}
