// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package configstore

import (
	"testing"

	"github.com/truvity/github-roster/pkg/config"
)

func TestSplitOrgParam(t *testing.T) {
	s := &OrgSSM{prefix: "/roster/orgs/"}

	for _, tc := range []struct {
		in                string
		name, team, field string
		ok                bool
	}{
		{"/roster/orgs/acme/minAdmins", "acme", "", "minAdmins", true},
		{"/roster/orgs/acme/teams/devs/groups", "acme", "devs", "groups", true},
		{"/roster/orgs/acme", "", "", "", false},              // no field
		{"/roster/orgs/acme/teams/devs", "", "", "", false},   // team without field
		{"/roster/orgs/acme/notteams/x/y", "", "", "", false}, // not the teams segment
		{"/other/prefix/x/y", "", "", "", false},              // outside the store
	} {
		name, team, field, ok := s.splitOrgParam(tc.in)
		if ok != tc.ok || name != tc.name || team != tc.team || field != tc.field {
			t.Errorf("splitOrgParam(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.in, name, team, field, ok, tc.name, tc.team, tc.field, tc.ok)
		}
	}
}

func TestOrgsFrom(t *testing.T) {
	byName := map[string]*collectedOrg{
		// Well-formed: credential pointer + a non-empty team.
		"acme": {
			scalars: map[string]string{
				fieldConsoleAppSSM: "/secrets/acme/console",
				fieldApplierAppSSM: "/secrets/acme/applier",
				fieldMinAdmins:     "2",
				fieldExceptions:    "bot-a, bot-b",
			},
			teams: map[string]map[string]string{
				"devs":  {fieldGroups: "g1@acme, g2@acme", fieldMembers: "m1"},
				"empty": {}, // dropped — no intent
			},
		},
		// Credential pointer but no team → dropped (would drive removals).
		"noteams": {
			scalars: map[string]string{fieldConsoleAppSSM: "/secrets/noteams/console"},
			teams:   map[string]map[string]string{},
		},
		// Team but no credential pointer → dropped (not a usable org).
		"nocreds": {
			scalars: map[string]string{},
			teams:   map[string]map[string]string{"devs": {fieldGroups: "g@x"}},
		},
	}

	got := orgsFrom(byName)

	if len(got) != 1 || got[0].Name != "acme" {
		t.Fatalf("orgsFrom = %+v, want only acme", got)
	}

	acme := got[0]
	if acme.ConsoleAppSSM != "/secrets/acme/console" || acme.ApplierAppSSM != "/secrets/acme/applier" {
		t.Errorf("app pointers = %q / %q", acme.ConsoleAppSSM, acme.ApplierAppSSM)
	}
	if acme.MinAdmins != 2 {
		t.Errorf("minAdmins = %d, want 2", acme.MinAdmins)
	}
	if len(acme.Exceptions) != 2 {
		t.Errorf("exceptions = %v, want 2", acme.Exceptions)
	}
	if acme.ReconcileEnabled {
		t.Error("a store org must be born reconcile-disabled")
	}
	if len(acme.Teams) != 1 { // "empty" dropped
		t.Fatalf("teams = %v, want only devs", acme.Teams)
	}
	if devs := acme.Teams["devs"]; len(devs.Groups) != 2 || len(devs.Members) != 1 {
		t.Errorf("devs team = %+v", devs)
	}
}

func TestMergeOrgs(t *testing.T) {
	iac := []config.Org{{Name: "acme"}}
	store := []config.Org{
		{Name: "Acme"}, // shadowed by git (case-insensitive)
		{Name: "beta"}, // store-only, appended
	}

	got := MergeOrgs(iac, store)

	if len(got) != 2 {
		t.Fatalf("merged = %d, want 2 (git acme wins over store Acme; beta appended)", len(got))
	}
	if got[0].Name != "acme" || got[1].Name != "beta" {
		t.Errorf("merged = %+v", got)
	}
}
