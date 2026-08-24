// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rosterv1 "github.com/truvity/github-roster/gen/roster/v1"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/version"
)

func TestRosterConnectGetVersion(t *testing.T) {
	s := &rosterConnect{deps: &Deps{Version: version.Info{Version: "1.2.3", Commit: "abcdef0"}}}

	resp, err := s.GetVersion(context.Background(), connect.NewRequest(&rosterv1.GetVersionRequest{}))
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}

	if got := resp.Msg.GetVersion(); got != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", got)
	}

	if got := resp.Msg.GetCommit(); got != "abcdef0" {
		t.Errorf("commit = %q, want abcdef0", got)
	}
}

func TestRosterConnectGetSettings(t *testing.T) {
	deps := &Deps{Config: &config.Config{
		Sources: []config.Source{
			{Name: "acme", Domains: []string{"acme.example"}, Endpoint: "http://ggs-acme"},
		},
		Orgs: []config.Org{{
			Name: "org1", Company: "co", MinAdmins: 2, ReconcileEnabled: true,
			Teams: map[string]config.Team{
				"devs": {Groups: []string{"g1@acme.example"}, Members: []string{"m1"}, Pinned: true},
			},
		}},
	}}
	s := &rosterConnect{deps: deps}

	resp, err := s.GetSettings(context.Background(), connect.NewRequest(&rosterv1.GetSettingsRequest{}))
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}

	if len(resp.Msg.GetSources()) != 1 || resp.Msg.GetSources()[0].GetName() != "acme" ||
		resp.Msg.GetSources()[0].GetEndpoint() != "http://ggs-acme" {
		t.Fatalf("sources = %+v", resp.Msg.GetSources())
	}

	if len(resp.Msg.GetOrgs()) != 1 {
		t.Fatalf("orgs = %d, want 1", len(resp.Msg.GetOrgs()))
	}

	o := resp.Msg.GetOrgs()[0]
	if o.GetName() != "org1" || o.GetCompany() != "co" || o.GetMinAdmins() != 2 || !o.GetReconcileEnabled() {
		t.Errorf("org = %+v", o)
	}

	if len(o.GetTeams()) != 1 || o.GetTeams()[0].GetName() != "devs" || !o.GetTeams()[0].GetPinned() {
		t.Errorf("teams = %+v", o.GetTeams())
	}
}

// TestRosterConnectGuards: each new method errors (not panics) when its
// dependency is absent, so a misconfigured deployment fails cleanly.
func TestRosterConnectGuards(t *testing.T) {
	s := &rosterConnect{deps: &Deps{}} // no Mapping, Broker or Audit

	if _, err := s.GetStatus(context.Background(), connect.NewRequest(&rosterv1.GetStatusRequest{})); err == nil {
		t.Error("GetStatus without a broker should error")
	}

	if _, err := s.GetAudit(context.Background(), connect.NewRequest(&rosterv1.GetAuditRequest{})); err == nil {
		t.Error("GetAudit without an audit sink should error")
	}

	if _, err := s.GetRoster(context.Background(), connect.NewRequest(&rosterv1.GetRosterRequest{})); err == nil {
		t.Error("GetRoster without a mapping store should error")
	}
}
