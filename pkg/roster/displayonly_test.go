// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

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

// A config whose directory serves one synced and one display-only domain.
const displayDoc = `
oidc: {disabled: true}
companies:
  corp:
    directory:
      ssmPrefix: /secrets/directory/corp
      domains:
        - domain: example.com
        - domain: shown.example
          sync: false
    github:
      org: example-org
      consoleAppSSM: /secrets/roster/console/example-org
      applierAppSSM: /secrets/roster/applier/example-org
      teams:
        team-engineers:
          groups: [team-engineers@example.com]
        team-shown:
          groups: [team-shown@shown.example]
`

func displayJoin(t *testing.T) *roster.Roster {
	t.Helper()

	parsed, err := config.Parse([]byte(displayDoc))
	require.NoError(t, err)

	snap := &directory.Snapshot{
		Source: "corp",
		Users: []directory.User{
			// Dora exists ONLY in the display-only domain, live, and the
			// synced-domain group lists her: nothing may be granted.
			{Name: "Display Dora", Email: "dora@shown.example", Live: true},
			// Sam exists ONLY in the display-only domain and is SUSPENDED
			// there — the case that must never read as a leaver.
			{Name: "Sus Sam", Email: "sam@shown.example", Live: false},
			// Max has a live synced identity too: fully normal behavior.
			{Name: "Mixed Max", Email: "max@example.com", Live: true},
		},
		Groups: map[string][]string{
			"team-engineers@example.com": {"dora@shown.example", "max@example.com"},
			"team-shown@shown.example":   {"max@example.com"},
		},
		FetchedAt: time.Now(),
	}

	state := &orgstate.State{
		Org: "example-org",
		Members: []orgstate.Member{
			{Login: "dora", Role: orgstate.RoleMember},
			{Login: "sam", Role: orgstate.RoleMember},
			{Login: "max", Role: orgstate.RoleMember},
		},
		Teams:       []orgstate.Team{{Slug: "team-engineers"}, {Slug: "team-shown"}},
		TeamMembers: map[string][]string{"team-engineers": {"max"}, "team-shown": {}},
	}

	return roster.Join(roster.Inputs{
		Config:         parsed,
		Snapshots:      []*directory.Snapshot{snap},
		SourceStatuses: []directory.Status{{Source: "corp", Healthy: true, Ready: true}},
		Entries: []mapping.Entry{
			{Name: "Display Dora", GitHub: "dora", Class: mapping.ClassEmployee},
			{Name: "Sus Sam", GitHub: "sam", Class: mapping.ClassEmployee},
			{Name: "Mixed Max", GitHub: "max", Class: mapping.ClassEmployee},
		},
		Orgs: map[string]*orgstate.State{"example-org": state},
		Now:  time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
	})
}

// A person known only to a display-only domain is shown but granted nothing —
// even when a synced-domain group lists them.
func TestDisplayOnlyPersonGetsNoGrants(t *testing.T) {
	t.Parallel()

	dora := person(t, displayJoin(t), "Display Dora")

	require.True(t, dora.DisplayOnly)
	require.True(t, dora.Live, "display liveness is the truth the UI shows")
	require.Equal(t, []string{"corp"}, dora.Sources, "still traced to her directory")

	m := dora.Orgs["example-org"]
	require.True(t, m.Member)
	require.Empty(t, m.DesiredTeams, "a synced-domain group listing a display-only person must not grant")
	require.Equal(t, roster.StateDisplayOnly, m.State)
	require.Equal(t, roster.PersonDisplayOnly, dora.State)
}

// Suspension in a display-only domain must never read as a leaver: the state
// stays display-only, not leaving.
func TestDisplayOnlySuspensionIsNotLeaving(t *testing.T) {
	t.Parallel()

	sam := person(t, displayJoin(t), "Sus Sam")

	require.True(t, sam.DisplayOnly)

	m := sam.Orgs["example-org"]
	require.True(t, m.Member)
	require.False(t, m.Live, "display truth: suspended")
	require.NotEqual(t, roster.StateLeaving, m.State, "a display-only suspension must not become a removal")
	require.Equal(t, roster.StateDisplayOnly, m.State)
}

// A group living in a display-only domain grants nothing, even to a person
// with a live synced identity; their synced-domain group still works.
func TestDisplayOnlyGroupGrantsNothing(t *testing.T) {
	t.Parallel()

	mixed := person(t, displayJoin(t), "Mixed Max")

	require.False(t, mixed.DisplayOnly, "a live synced identity keeps the person fully governed")

	m := mixed.Orgs["example-org"]
	require.Equal(t, []string{"team-engineers"}, m.DesiredTeams,
		"synced-domain group grants; display-only-domain group (team-shown) must not")
	require.Equal(t, roster.StateSynced, m.State)
}
