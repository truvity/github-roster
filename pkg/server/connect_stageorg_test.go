// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rosterv1 "github.com/truvity/github-roster/gen/roster/v1"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/configstore"
)

type fakeOrgStore struct {
	staged config.Org
	calls  int
	// team edits recorded by PutTeam/DeleteTeam
	putOrg, putTeam string
	putCfg          config.Team
	deleted         string
}

func (f *fakeOrgStore) ListOrgs(context.Context) ([]config.Org, error) { return nil, nil }
func (f *fakeOrgStore) PutOrg(_ context.Context, org config.Org) error {
	f.staged = org
	f.calls++
	return nil
}
func (f *fakeOrgStore) PutApp(context.Context, string, configstore.AppCredentials) error {
	return nil
}
func (f *fakeOrgStore) PutProvenance(context.Context, string, string) error { return nil }
func (f *fakeOrgStore) PutTeam(_ context.Context, org, team string, cfg config.Team) error {
	f.putOrg, f.putTeam, f.putCfg = org, team, cfg

	return nil
}
func (f *fakeOrgStore) DeleteTeam(_ context.Context, org, team string) error {
	f.deleted = org + "/" + team

	return nil
}

func operatorCtx() context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleOperator})
}

func TestStageOrgStoresForOperator(t *testing.T) {
	store := &fakeOrgStore{}
	s := &rosterConnect{deps: &Deps{OrgStore: store}}

	_, err := s.StageOrg(operatorCtx(), connect.NewRequest(&rosterv1.StageOrgRequest{
		Name: "acme", Team: "devs", Groups: []string{" g@acme ", ""}, MinAdmins: 2,
	}))
	if err != nil {
		t.Fatalf("StageOrg: %v", err)
	}

	if store.staged.Name != "acme" || store.staged.MinAdmins != 2 {
		t.Fatalf("staged = %+v", store.staged)
	}

	if g := store.staged.Teams["devs"].Groups; len(g) != 1 || g[0] != "g@acme" {
		t.Errorf("groups = %v, want [g@acme] (trimmed, empties dropped)", g)
	}
}

func TestStageOrgRejectsViewer(t *testing.T) {
	s := &rosterConnect{deps: &Deps{OrgStore: &fakeOrgStore{}}}

	ctx := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleViewer})

	_, err := s.StageOrg(ctx, connect.NewRequest(&rosterv1.StageOrgRequest{
		Name: "acme", Team: "devs", Groups: []string{"g@acme"},
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer StageOrg code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}

func TestStageOrgRejectsIncomplete(t *testing.T) {
	store := &fakeOrgStore{}
	s := &rosterConnect{deps: &Deps{OrgStore: store}}

	// A team with no group and no member is refused (it would drive removals).
	_, err := s.StageOrg(operatorCtx(), connect.NewRequest(&rosterv1.StageOrgRequest{Name: "acme", Team: "devs"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("incomplete StageOrg code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	if store.calls != 0 {
		t.Errorf("PutOrg called %d times on invalid input, want 0", store.calls)
	}
}

// The hybrid model's boundary: team edits land on store orgs, and a
// git-declared org is refused — the git layer stays the reviewed baseline.
func TestPutOrgTeamEditsStoreOrgOnly(t *testing.T) {
	store := &fakeOrgStore{}
	cfg := &config.Config{Orgs: []config.Org{{Name: "git-org", Company: "co"}}}
	s := &rosterConnect{deps: &Deps{OrgStore: store, Config: cfg}}

	// Store org: the edit lands.
	if _, err := s.PutOrgTeam(operatorCtx(), connect.NewRequest(&rosterv1.PutOrgTeamRequest{
		Org: "acme-inc", Team: "team-eng", Groups: []string{" eng@acme.example ", ""},
	})); err != nil {
		t.Fatalf("PutOrgTeam: %v", err)
	}

	if store.putOrg != "acme-inc" || store.putTeam != "team-eng" ||
		len(store.putCfg.Groups) != 1 || store.putCfg.Groups[0] != "eng@acme.example" {
		t.Fatalf("stored = %q/%q %+v", store.putOrg, store.putTeam, store.putCfg)
	}

	// Git-declared org: refused, store untouched.
	prev := store.putTeam
	if _, err := s.PutOrgTeam(operatorCtx(), connect.NewRequest(&rosterv1.PutOrgTeamRequest{
		Org: "git-org", Team: "team-eng", Groups: []string{"g@co.example"},
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("git-declared org code = %v, want FailedPrecondition", connect.CodeOf(err))
	}

	if store.putTeam != prev {
		t.Fatal("a refused edit must not reach the store")
	}

	// Empty mapping: refused (delete is the way to unmap).
	if _, err := s.PutOrgTeam(operatorCtx(), connect.NewRequest(&rosterv1.PutOrgTeamRequest{
		Org: "acme-inc", Team: "team-eng",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty mapping code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestDeleteOrgTeamStoreOnly(t *testing.T) {
	store := &fakeOrgStore{}
	cfg := &config.Config{Orgs: []config.Org{{Name: "git-org", Company: "co"}}}
	s := &rosterConnect{deps: &Deps{OrgStore: store, Config: cfg}}

	if _, err := s.DeleteOrgTeam(operatorCtx(), connect.NewRequest(&rosterv1.DeleteOrgTeamRequest{
		Org: "acme-inc", Team: "team-eng",
	})); err != nil {
		t.Fatalf("DeleteOrgTeam: %v", err)
	}

	if store.deleted != "acme-inc/team-eng" {
		t.Fatalf("deleted = %q", store.deleted)
	}

	if _, err := s.DeleteOrgTeam(operatorCtx(), connect.NewRequest(&rosterv1.DeleteOrgTeamRequest{
		Org: "git-org", Team: "team-eng",
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("git-declared org code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}
