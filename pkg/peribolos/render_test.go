package peribolos_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/peribolos"
	"github.com/truvity/github-roster/pkg/roster"
)

const orgName = "example-org"

func orgConfig() *config.Org {
	return &config.Org{
		Name:       orgName,
		Exceptions: []string{"example-bot-app"},
		Teams: map[string]config.Team{
			"engineers": {Groups: []string{"engineers@example.com"}},
			"robots":    {Pinned: true},
		},
	}
}

// person builds a fixture whose per-org liveness matches the person-level
// value — the single-identity case. Dual-identity people, live overall
// but suspended for this org, set Membership.Live separately.
func person(name, login string, live bool, teams ...string) roster.Person {
	return roster.Person{
		Name:   name,
		GitHub: login,
		Class:  mapping.ClassEmployee,
		Live:   live,
		Orgs: map[string]roster.Membership{
			orgName: {Member: true, Live: live, DesiredTeams: teams},
		},
	}
}

func state(members ...orgstate.Member) *orgstate.State {
	return &orgstate.State{
		Org:         orgName,
		Members:     members,
		Teams:       []orgstate.Team{{Slug: "engineers"}, {Slug: "robots"}},
		TeamMembers: map[string][]string{},
	}
}

func member(login string) orgstate.Member {
	return orgstate.Member{Login: login, Role: orgstate.RoleMember}
}

func owner(login string) orgstate.Member {
	return orgstate.Member{Login: login, Role: orgstate.RoleAdmin}
}

func render(t *testing.T, in peribolos.Inputs) *peribolos.Result {
	t.Helper()

	result, err := peribolos.Render(in)
	require.NoError(t, err)

	return result
}

// The central invariant, stated as a test: an unattended run cannot add
// anybody, no matter what the desired state says.
func TestRemovalsOnlyNeverAdds(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode: peribolos.ModeRemovalsOnly,
		Org:  orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{
			person("Ada Lovelace", "ada", true, "engineers"),
			// Live, desired in a team, but NOT currently a member. A full
			// sync would invite them; this mode must not.
			person("New Joiner", "joiner", true, "engineers"),
		}},
		State: state(member("ada")),
	})

	require.Empty(t, result.Adding)
	require.NotContains(t, result.Document.Orgs[orgName].Members, "joiner")
	require.Contains(t, result.Document.Orgs[orgName].Members, "ada")
}

func TestRemovalsOnlyRemovesTheNotLive(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode: peribolos.ModeRemovalsOnly,
		Org:  orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{
			person("Ada Lovelace", "ada", true),
			person("Gone Person", "gone", false),
		}},
		State: state(member("ada"), member("gone")),
	})

	require.Equal(t, []string{"gone"}, result.Removing)
	require.Equal(t, []string{"ada"}, result.Document.Orgs[orgName].Members)
}

// Teams are omitted entirely: removing someone from the organization
// supersedes every team they were in, so the leaver SLA needs nothing more,
// and touching teams unattended would widen the surface for no gain.
func TestRemovalsOnlyOmitsTeams(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeRemovalsOnly,
		Org:    orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{person("Ada Lovelace", "ada", true, "engineers")}},
		State:  state(member("ada")),
	})

	require.Nil(t, result.Document.Orgs[orgName].Teams)
	require.NotContains(t, result.YAML, "teams:")
}

// Absence of evidence is not evidence. Someone nobody can identify is an
// operator's decision, never an unattended removal.
func TestUnidentifiedMembersAreNeverRemovedUnattended(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeRemovalsOnly,
		Org:    orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{person("Ada Lovelace", "ada", true)}},
		State:  state(member("ada"), member("stranger")),
	})

	require.Empty(t, result.Removing)
	require.Contains(t, result.Document.Orgs[orgName].Members, "stranger")
}

func TestExceptionsAreNeverRemoved(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode: peribolos.ModeRemovalsOnly,
		Org:  orgConfig(),
		// Even mapped and not live, an exception stays.
		Roster: &roster.Roster{People: []roster.Person{person("Bot App", "example-bot-app", false)}},
		State:  state(member("example-bot-app")),
	})

	require.Empty(t, result.Removing)
	require.Contains(t, result.Document.Orgs[orgName].Members, "example-bot-app")
}

// A directory that did not answer must not cost anyone their access.
func TestUnhealthySourceSuppressesRemoval(t *testing.T) {
	t.Parallel()

	gone := person("Gone Person", "gone", false)
	gone.Sources = []string{"corp"}

	result := render(t, peribolos.Inputs{
		Mode:             peribolos.ModeRemovalsOnly,
		Org:              orgConfig(),
		Roster:           &roster.Roster{People: []roster.Person{gone}},
		State:            state(member("gone")),
		UnhealthySources: []string{"corp"},
	})

	require.Empty(t, result.Removing, "a person known only to an unhealthy directory must be left alone")
	require.Contains(t, result.Notes[0], "corp is unhealthy")
	require.Equal(t, []string{"corp"}, result.SkippedSources)
}

// The 2026-07-31 near-miss: a person whose ONLY declared directory has
// never produced a snapshot has empty Sources — presence-based protection
// alone let them look removable. The entry's email domains name the
// EXPECTED source, and an unhealthy expected source protects them the
// same way a stale one does.
func TestExpectedSourceUnhealthySuppressesRemoval(t *testing.T) {
	t.Parallel()

	partner := person("Partner Person", "partner", false)
	partner.Sources = nil
	partner.ExpectedSources = []string{"partnerdir"}

	result := render(t, peribolos.Inputs{
		Mode:             peribolos.ModeRemovalsOnly,
		Org:              orgConfig(),
		Roster:           &roster.Roster{People: []roster.Person{partner}},
		State:            state(member("partner")),
		UnhealthySources: []string{"partnerdir"},
	})

	require.Empty(t, result.Removing,
		"a person expected only in a never-read directory must be left alone")
}

// Dropping an invited person cancels an invitation they are about to
// accept, and they see it happen.
func TestPendingInvitationsSurviveBothModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []peribolos.Mode{peribolos.ModeRemovalsOnly, peribolos.ModeFull} {
		st := state(member("ada"))
		st.Invitations = []orgstate.Invitation{{Login: "invited"}}

		result := render(t, peribolos.Inputs{
			Mode:   mode,
			Org:    orgConfig(),
			Roster: &roster.Roster{People: []roster.Person{person("Ada Lovelace", "ada", true)}},
			State:  st,
		})

		require.Contains(t, result.Document.Orgs[orgName].Members, "invited",
			"mode %s dropped a pending invitation", mode)
		require.NotContains(t, result.Removing, "invited")
	}
}

// Owners are registry-pinned and change by reviewed commit. Omitting them
// would propose removing every one.
func TestOwnersArePassedThroughUnchanged(t *testing.T) {
	t.Parallel()

	for _, mode := range []peribolos.Mode{peribolos.ModeRemovalsOnly, peribolos.ModeFull} {
		result := render(t, peribolos.Inputs{
			Mode: mode,
			Org:  orgConfig(),
			// The owner is not live, and still must not be touched here.
			Roster: &roster.Roster{People: []roster.Person{person("Departed Owner", "boss", false)}},
			State:  state(owner("boss"), member("ada")),
		})

		require.Equal(t, []string{"boss"}, result.Document.Orgs[orgName].Admins, "mode %s", mode)
		require.NotContains(t, result.Removing, "boss")
	}
}

func TestFullSyncAddsAndRemoves(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode: peribolos.ModeFull,
		Org:  orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{
			person("Ada Lovelace", "ada", true, "engineers"),
			joinerWantingTeam("New Joiner", "joiner"),
			person("Gone Person", "gone", false),
		}},
		State: state(member("ada"), member("gone")),
	})

	require.Equal(t, []string{"joiner"}, result.Adding)
	require.Equal(t, []string{"gone"}, result.Removing)
}

// A configured team that does not exist on GitHub is skipped: creating one
// belongs to the structure engine, and this service must never invent one.
func TestTeamsThatDoNotExistAreSkipped(t *testing.T) {
	t.Parallel()

	st := state(member("ada"))
	st.Teams = []orgstate.Team{{Slug: "engineers"}} // no "robots"

	result := render(t, peribolos.Inputs{
		Mode: peribolos.ModeFull,
		Org:  orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{
			person("Ada Lovelace", "ada", true, "engineers", "robots"),
		}},
		State: st,
	})

	teams := result.Document.Orgs[orgName].Teams
	require.Contains(t, teams, "engineers")
	require.NotContains(t, teams, "robots")
}

func TestFullSyncRendersTeamMembership(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode: peribolos.ModeFull,
		Org:  orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{
			person("Ada Lovelace", "ada", true, "engineers"),
			person("Bot", "bot", true, "robots"),
		}},
		State: state(member("ada"), member("bot")),
	})

	teams := result.Document.Orgs[orgName].Teams
	require.Equal(t, []string{"ada"}, teams["engineers"].Members)
	require.Equal(t, []string{"bot"}, teams["robots"].Members)
}

// Even an operator sync leaves unidentified members alone: removing them is
// a deliberate act, not a side effect of pressing Sync.
func TestFullSyncLeavesUnidentifiedMembersInPlace(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeFull,
		Org:    orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{person("Ada Lovelace", "ada", true, "engineers")}},
		State:  state(member("ada"), member("stranger")),
	})

	require.Empty(t, result.Removing)
	require.Contains(t, result.Document.Orgs[orgName].Members, "stranger")
	require.Contains(t, result.Notes[0], "remove them deliberately")
}

func TestRenderedYAMLShape(t *testing.T) {
	t.Parallel()

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeFull,
		Org:    orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{person("Ada Lovelace", "ada", true, "engineers")}},
		State:  state(owner("boss"), member("ada")),
	})

	require.Contains(t, result.YAML, "orgs:")
	require.Contains(t, result.YAML, "example-org:")
	require.Contains(t, result.YAML, "admins:")
	require.Contains(t, result.YAML, "- boss")
	require.Contains(t, result.YAML, "- ada")
}

func TestUnknownModeIsRefused(t *testing.T) {
	t.Parallel()

	_, err := peribolos.Render(peribolos.Inputs{
		Mode:   "whatever",
		Org:    orgConfig(),
		Roster: &roster.Roster{},
		State:  state(),
	})

	require.ErrorContains(t, err, "unknown render mode")
}

func TestMissingInputsAreRefused(t *testing.T) {
	t.Parallel()

	_, err := peribolos.Render(peribolos.Inputs{Mode: peribolos.ModeFull})
	require.Error(t, err)
}

// joinerWantingTeam is live, desired in a team, and not yet a member.
func joinerWantingTeam(name, login string) roster.Person {
	p := person(name, login, true, "engineers")
	p.Orgs[orgName] = roster.Membership{Member: false, Live: true, DesiredTeams: []string{"engineers"}}

	return p
}

// A pinned team's residents include logins no mapping describes — bots,
// App accounts. The render must keep them: pins may add, and removal from
// a pinned team is a manual act, never a sync side effect.
func TestPinnedTeamKeepsCurrentMembers(t *testing.T) {
	t.Parallel()

	st := state(member("ada"))
	st.TeamMembers["robots"] = []string{"example-bot-app"}

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeFull,
		Org:    orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{person("Ada Lovelace", "ada", true, "engineers")}},
		State:  st,
	})

	require.Equal(t, []string{"example-bot-app"},
		result.Document.Orgs[orgName].Teams["robots"].Members)
	require.Contains(t, result.Notes[0], "pinned")
}

// A pin can still add someone to a pinned team; the current residents ride
// along.
func TestPinnedTeamUnionsPinsWithCurrent(t *testing.T) {
	t.Parallel()

	st := state(member("ada"), member("bot"))
	st.TeamMembers["robots"] = []string{"example-bot-app"}

	result := render(t, peribolos.Inputs{
		Mode: peribolos.ModeFull,
		Org:  orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{
			person("Bot", "bot", true, "robots"),
		}},
		State: st,
	})

	require.Equal(t, []string{"bot", "example-bot-app"},
		result.Document.Orgs[orgName].Teams["robots"].Members)
}

// An absent backing group suppresses removals, never explicit additions:
// the group contributes nobody, so a members: list must still fill the
// team while current members stay protected. (2026-08-01: the trust-form
// pre-ENG-52 fill rendered no team-adds because the fail-safe skipped
// the explicit list entirely.)
func TestAbsentGroupKeepsCurrentButExplicitMembersStillAdd(t *testing.T) {
	t.Parallel()

	org := orgConfig()
	org.Teams["engineers"] = config.Team{
		Groups:  []string{"engineers@example.com"},
		Members: []string{"ada@example.com"},
	}

	st := state(member("ada"), member("veteran"))
	st.TeamMembers["engineers"] = []string{"veteran"}

	r := &roster.Roster{
		People:       []roster.Person{person("Ada Lovelace", "ada", true, "engineers")},
		AbsentGroups: []string{"engineers@example.com"},
	}

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeFull,
		Org:    org,
		Roster: r,
		State:  st,
	})

	members := result.Document.Orgs[orgName].Teams["engineers"].Members
	require.Contains(t, members, "ada", "the explicit addition must survive the absent-group fail-safe")
	require.Contains(t, members, "veteran", "current members must be kept while the group is absent")
}

// A person suspended in THIS org's own company is a leaver here even
// while another company keeps them live: the unattended run evicts them
// from this org alone — person-level liveness no longer shields the
// membership, the per-org value decides.
func TestRemovalsOnlyEvictsPersonSuspendedForThisOrg(t *testing.T) {
	t.Parallel()

	half := person("Maks Ustinov", "maks", true)
	half.Orgs[orgName] = roster.Membership{Member: true, Live: false}

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeRemovalsOnly,
		Org:    orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{half}},
		State:  state(member("maks")),
	})

	require.Equal(t, []string{"maks"}, result.Removing)
}

// The operator sync waives the same membership: live-elsewhere does not
// keep a seat in the org whose own company suspended the person.
func TestFullSyncEvictsPersonSuspendedForThisOrg(t *testing.T) {
	t.Parallel()

	half := person("Maks Ustinov", "maks", true)
	half.Orgs[orgName] = roster.Membership{Member: true, Live: false}

	result := render(t, peribolos.Inputs{
		Mode:   peribolos.ModeFull,
		Org:    orgConfig(),
		Roster: &roster.Roster{People: []roster.Person{half}},
		State:  state(member("maks")),
	})

	require.Empty(t, result.Adding)
	require.Equal(t, []string{"maks"}, result.Removing)
}
