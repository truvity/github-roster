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

	// A live member whose teams match desired is synced — and the derived
	// state must actually be wired into the join (regression: membershipState
	// was defined but never called, leaving every state empty so the console's
	// Active/Left filters matched nobody).
	require.Equal(t, roster.StateSynced, m.State)
	require.Equal(t, roster.PersonSynced, ada.State)
}

// Membership in a team the roster does NOT manage must not make a person look
// "pending": planTeams only ever touches configured teams, so an unmanaged
// team (a CI/infra team the config never names) is outside scope. Comparing it
// would leave the person pending forever against a change that never happens.
func TestUnmanagedTeamMembershipDoesNotBlockSynced(t *testing.T) {
	t.Parallel()

	state := orgState()
	// Ada is also in "role-ci", which the config never names.
	state.Teams = append(state.Teams, orgstate.Team{Slug: "role-ci"})
	state.TeamMembers["role-ci"] = []string{"ada"}

	r := roster.Join(roster.Inputs{
		Config:         cfg(t),
		Snapshots:      []*directory.Snapshot{snapshot()},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries:        entries(),
		Orgs:           map[string]*orgstate.State{"example-org": state},
		Now:            time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})

	ada := person(t, r, "Ada Lovelace")
	m := ada.Orgs["example-org"]

	require.NotContains(t, m.Teams, "role-ci", "unmanaged team must not appear in the person's tracked teams")
	require.Equal(t, roster.StateSynced, m.State, "an unmanaged-team membership must not force pending")
	require.Equal(t, roster.PersonSynced, ada.State)
}

// An invited person occupies a seat and is absent from the members API.
// Counting them as a non-member sends a second invitation; counting them as
// a plain member hides that they have not accepted.
func TestPendingInvitationCountsAsMembership(t *testing.T) {
	t.Parallel()

	turing := person(t, join(t), "Alan Turing")
	m := turing.Orgs["example-org"]

	require.True(t, m.Member)
	require.True(t, m.InvitationPending)
	require.Equal(t, roster.StateInvited, m.State)
	require.Equal(t, roster.PersonInvited, turing.State)
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

// The 2026-08-03 incident: a freshly mapped, live person no team claims
// produces hash-identical sync plans, and nothing said why. The join now
// flags them — on the person and as a warning.
func TestNoTeamPersonIsFlaggedAndReported(t *testing.T) {
	t.Parallel()

	snap := snapshot()
	snap.Users = append(snap.Users, directory.User{
		Name: "New Hire", Email: "new.hire@example.com", Live: true,
	})

	r := roster.Join(roster.Inputs{
		Config:         cfg(t),
		Snapshots:      []*directory.Snapshot{snap},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries: append(entries(),
			mapping.Entry{Name: "New Hire", GitHub: "new-hire", Class: mapping.ClassEmployee}),
		Orgs: map[string]*orgstate.State{"example-org": orgState()},
		Now:  time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	})

	hire := person(t, r, "New Hire")
	require.True(t, hire.Live)
	require.True(t, hire.NoTeam)
	require.Empty(t, hire.DesiredAnywhere())

	found := warnings(r, roster.WarnNoTeam)
	require.Len(t, found, 1)
	require.Equal(t, "New Hire", found[0].Subject)

	// People with teams are not flagged.
	require.False(t, person(t, r, "Ada Lovelace").NoTeam)
	require.Equal(t, []string{"example-org/team-engineers"},
		person(t, r, "Ada Lovelace").DesiredAnywhere())
}

// A suspended person is desired in no team BY DESIGN (the removals path);
// warning about them would bury the signal under every leaver. An orphaned
// entry already has its own warning with a different fix.
func TestNoTeamWarningSkipsSuspendedAndOrphaned(t *testing.T) {
	t.Parallel()

	r := join(t)

	require.True(t, person(t, r, "Gone Person").NoTeam)
	require.True(t, person(t, r, "Ghost Entry").NoTeam)
	require.Empty(t, warnings(r, roster.WarnNoTeam))
}

// A bot no pin claims is as invisible to every sync as an unclaimed
// employee — and it has no directory to claim it any other way.
func TestUnpinnedBotIsReportedAsNoTeam(t *testing.T) {
	t.Parallel()

	r := roster.Join(roster.Inputs{
		Config:         cfg(t),
		Snapshots:      []*directory.Snapshot{snapshot()},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries: append(entries(),
			mapping.Entry{Name: "Stray Bot", GitHub: "stray-bot", Class: mapping.ClassBot}),
		Orgs: map[string]*orgstate.State{"example-org": orgState()},
		Now:  time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	})

	found := warnings(r, roster.WarnNoTeam)
	require.Len(t, found, 1)
	require.Equal(t, "Stray Bot", found[0].Subject)
}

// ---- dual-identity cases: one person, an account in each of two ----
// ---- companies, each company owning one organization             ----

const dualDoc = `
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
      teams:
        team-engineers:
          groups: [team-engineers@example.com]
  partner:
    directory:
      ssmPrefix: /secrets/directory/partner
      domains: [partner.example]
    github:
      org: partner-org
      consoleAppSSM: /secrets/roster/console/partner-org
      applierAppSSM: /secrets/roster/applier/partner-org
      teams:
        team-wallet:
          groups: [team-wallet@partner.example]
        team-listed:
          members: [maks@partner.example]
`

func dualCfg(t *testing.T) *config.Config {
	t.Helper()

	parsed, err := config.Parse([]byte(dualDoc))
	require.NoError(t, err)

	return parsed
}

// dualJoin joins one dual-identity person whose corp and partner
// accounts carry the given liveness. The partner directory lists the
// partner address in team-wallet's group either way — group membership
// routinely outlives suspension.
func dualJoin(t *testing.T, corpLive, partnerLive bool) *roster.Roster {
	t.Helper()

	return roster.Join(roster.Inputs{
		Config: dualCfg(t),
		Snapshots: []*directory.Snapshot{
			{
				Source:    "corp",
				Users:     []directory.User{{Name: "Maks Ustinov", Email: "maks@example.com", Live: corpLive}},
				Groups:    map[string][]string{"team-engineers@example.com": {"maks@example.com"}},
				FetchedAt: time.Now(),
			},
			{
				Source:    "partner",
				Users:     []directory.User{{Name: "Maks Ustinov", Email: "maks@partner.example", Live: partnerLive}},
				Groups:    map[string][]string{"team-wallet@partner.example": {"maks@partner.example"}},
				FetchedAt: time.Now(),
			},
		},
		SourceStatuses: []directory.Status{
			{Source: "corp", Healthy: true, Ready: true},
			{Source: "partner", Healthy: true, Ready: true},
		},
		Entries: []mapping.Entry{{
			Name:   "Maks Ustinov",
			GitHub: "maks",
			K8s:    "musti",
			Class:  mapping.ClassEmployee,
			// Suspended-company address declared FIRST, so the email
			// anchor test below proves ordering does not decide it.
			Emails: []string{"maks@partner.example", "maks@example.com"},
		}},
		Orgs: map[string]*orgstate.State{},
		Now:  time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	})
}

// The headline dual-identity case: live in corp, suspended in partner.
// The suspension is a leaver event for the partner org alone — its
// group AND its members: listing both name the suspended address, and
// neither grants anything — while the corp side keeps everything.
func TestDualIdentitySuspendedInOneCompany(t *testing.T) {
	t.Parallel()

	maks := person(t, dualJoin(t, true, false), "Maks Ustinov")

	require.True(t, maks.Live, "one live account keeps the person")

	corp := maks.Orgs["example-org"]
	require.True(t, corp.Live)
	require.Equal(t, []string{"team-engineers"}, corp.DesiredTeams)

	partner := maks.Orgs["partner-org"]
	require.False(t, partner.Live, "suspended in the org's own company is a leaver THERE")
	require.Empty(t, partner.DesiredTeams,
		"neither the suspended account's group nor a members: listing of its address may grant a team")
}

// Suspended everywhere is the plain leaver: nothing anywhere.
func TestDualIdentityAllSuspended(t *testing.T) {
	t.Parallel()

	maks := person(t, dualJoin(t, false, false), "Maks Ustinov")

	require.False(t, maks.Live)
	require.False(t, maks.Orgs["example-org"].Live)
	require.False(t, maks.Orgs["partner-org"].Live)
	require.Empty(t, maks.DesiredAnywhere())
}

// Both accounts live: both identities grant, nothing is waived.
func TestDualIdentityBothLive(t *testing.T) {
	t.Parallel()

	maks := person(t, dualJoin(t, true, true), "Maks Ustinov")

	require.True(t, maks.Live)
	require.True(t, maks.Orgs["example-org"].Live)
	require.True(t, maks.Orgs["partner-org"].Live)
	require.ElementsMatch(t,
		[]string{"example-org/team-engineers", "partner-org/team-listed", "partner-org/team-wallet"},
		maks.DesiredAnywhere())
}

// The display anchor prefers a live account's address whatever order the
// operator declared the emails in — a suspended address as the visible
// anchor misleads exactly the triage it exists for.
func TestDualIdentityEmailAnchorPrefersLive(t *testing.T) {
	t.Parallel()

	maks := person(t, dualJoin(t, true, false), "Maks Ustinov")
	require.Equal(t, "maks@example.com", maks.Email)
}

// The partner rail must survive the per-account gating: a corp person in
// a partner group under their LIVE corp address has no partner identity
// at all, so partner-org standing follows their home directory.
func TestCrossCompanyMembershipFollowsHomeDirectory(t *testing.T) {
	t.Parallel()

	r := roster.Join(roster.Inputs{
		Config: dualCfg(t),
		Snapshots: []*directory.Snapshot{
			{
				Source:    "corp",
				Users:     []directory.User{{Name: "Ada Lovelace", Email: "ada@example.com", Live: true}},
				Groups:    map[string][]string{"team-engineers@example.com": {"ada@example.com"}},
				FetchedAt: time.Now(),
			},
			{
				Source:    "partner",
				Users:     []directory.User{},
				Groups:    map[string][]string{"team-wallet@partner.example": {"ada@example.com"}},
				FetchedAt: time.Now(),
			},
		},
		SourceStatuses: []directory.Status{
			{Source: "corp", Healthy: true, Ready: true},
			{Source: "partner", Healthy: true, Ready: true},
		},
		Entries: []mapping.Entry{{
			Name: "Ada Lovelace", GitHub: "ada", Class: mapping.ClassEmployee,
			Emails: []string{"ada@example.com"},
		}},
		Orgs: map[string]*orgstate.State{},
		Now:  time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	})

	ada := person(t, r, "Ada Lovelace")
	require.True(t, ada.Orgs["partner-org"].Live, "no partner identity: home directory governs")
	require.Contains(t, ada.Orgs["partner-org"].DesiredTeams, "team-wallet",
		"a live account's cross-company group membership still grants")
}

// An owner suspended in THIS org's company is reported even while
// another company keeps them live — and not reported where they are.
func TestHalfSuspendedOwnerIsReportedPerOrg(t *testing.T) {
	t.Parallel()

	state := &orgstate.State{
		Org:     "partner-org",
		Members: []orgstate.Member{{Login: "maks", Role: orgstate.RoleAdmin}},
	}

	r := roster.Join(roster.Inputs{
		Config: dualCfg(t),
		Snapshots: []*directory.Snapshot{
			{
				Source:    "corp",
				Users:     []directory.User{{Name: "Maks Ustinov", Email: "maks@example.com", Live: true}},
				Groups:    map[string][]string{},
				FetchedAt: time.Now(),
			},
			{
				Source:    "partner",
				Users:     []directory.User{{Name: "Maks Ustinov", Email: "maks@partner.example", Live: false}},
				Groups:    map[string][]string{},
				FetchedAt: time.Now(),
			},
		},
		SourceStatuses: []directory.Status{
			{Source: "corp", Healthy: true, Ready: true},
			{Source: "partner", Healthy: true, Ready: true},
		},
		Entries: []mapping.Entry{{
			Name: "Maks Ustinov", GitHub: "maks", Class: mapping.ClassEmployee,
			Emails: []string{"maks@example.com", "maks@partner.example"},
		}},
		Orgs: map[string]*orgstate.State{"partner-org": state},
		Now:  time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	})

	found := warnings(r, roster.WarnNotLiveOwner)
	require.Len(t, found, 1)
	require.Equal(t, "maks", found[0].Subject)
	require.Equal(t, "partner-org", found[0].Org)
}
