// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package configstore

import (
	"context"
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

func TestOrgAppPath(t *testing.T) {
	s := &OrgSSM{prefix: "/roster/orgs/"}
	if got := s.appPath("acme", fieldPrivateKey); got != "/roster/orgs/acme/app/github-private-key" {
		t.Errorf("appPath = %q", got)
	}
}

func TestOrgParams(t *testing.T) {
	s := &OrgSSM{prefix: "/roster/orgs/"}

	params := s.orgParams(config.Org{
		Name:      "acme",
		MinAdmins: 2,
		Teams: map[string]config.Team{
			"devs": {Groups: []string{"g1@acme", "g2@acme"}, Members: []string{"m1"}},
		},
	})

	got := map[string]string{}
	for _, p := range params {
		got[p.name] = p.value
	}

	// The credential pointer is what makes orgsFrom keep the org, and it must
	// point at the org's own app/ path (where PutApp writes and the reader
	// reads).
	if got["/roster/orgs/acme/consoleAppSSM"] != "/roster/orgs/acme/app" {
		t.Errorf("consoleAppSSM = %q, want /roster/orgs/acme/app", got["/roster/orgs/acme/consoleAppSSM"])
	}
	if got["/roster/orgs/acme/minAdmins"] != "2" {
		t.Errorf("minAdmins = %q", got["/roster/orgs/acme/minAdmins"])
	}
	if got["/roster/orgs/acme/teams/devs/groups"] != "g1@acme,g2@acme" {
		t.Errorf("groups = %q", got["/roster/orgs/acme/teams/devs/groups"])
	}
	if got["/roster/orgs/acme/teams/devs/members"] != "m1" {
		t.Errorf("members = %q", got["/roster/orgs/acme/teams/devs/members"])
	}

	// Round-trip: the params PutOrg writes, read back through splitOrgParam +
	// orgsFrom, yield a usable org — the pointer survives the safety filter.
	byName := map[string]*collectedOrg{}
	for _, p := range params {
		name, team, field, ok := s.splitOrgParam(p.name)
		if !ok {
			continue
		}
		c := byName[name]
		if c == nil {
			c = &collectedOrg{scalars: map[string]string{}, teams: map[string]map[string]string{}}
			byName[name] = c
		}
		if team == "" {
			c.scalars[field] = p.value
			continue
		}
		if c.teams[team] == nil {
			c.teams[team] = map[string]string{}
		}
		c.teams[team][field] = p.value
	}

	orgs := orgsFrom(byName)
	if len(orgs) != 1 || orgs[0].Name != "acme" || orgs[0].MinAdmins != 2 {
		t.Fatalf("round-trip orgsFrom = %+v", orgs)
	}
	if orgs[0].Provenance != ProvenanceManual {
		t.Errorf("round-trip provenance = %q, want manual (untagged staged org)", orgs[0].Provenance)
	}
}

func TestPutOrgValidation(t *testing.T) {
	s := &OrgSSM{prefix: "/roster/orgs/"} // nil client: rejects before any write

	if err := s.PutOrg(context.Background(), config.Org{Name: ""}); err == nil {
		t.Error("PutOrg with no name should error")
	}

	if err := s.PutOrg(context.Background(), config.Org{
		Name:  "acme",
		Teams: map[string]config.Team{"empty": {}},
	}); err == nil {
		t.Error("PutOrg with only an empty team should error (would drive removals)")
	}
}

func TestOrgProvenance(t *testing.T) {
	base := func(prov string) *collectedOrg {
		c := &collectedOrg{
			scalars: map[string]string{fieldConsoleAppSSM: "/s/console"},
			teams:   map[string]map[string]string{"devs": {fieldGroups: "g@x"}},
		}
		if prov != "" {
			c.scalars[fieldProvenance] = prov
		}
		return c
	}

	// No tag → adopted (manual). Explicit roster is preserved.
	got := orgsFrom(map[string]*collectedOrg{"a": base(""), "b": base(ProvenanceRoster)})
	by := map[string]string{}
	for _, o := range got {
		by[o.Name] = o.Provenance
	}
	if by["a"] != ProvenanceManual {
		t.Errorf("untagged store org provenance = %q, want manual", by["a"])
	}
	if by["b"] != ProvenanceRoster {
		t.Errorf("tagged org provenance = %q, want roster", by["b"])
	}
}
