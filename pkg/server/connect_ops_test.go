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
)

type fakeDirStore struct {
	put  config.Source
	puts int
}

func (f *fakeDirStore) List(context.Context) ([]config.Source, error) { return nil, nil }
func (f *fakeDirStore) Put(_ context.Context, src config.Source) error {
	f.put = src
	f.puts++

	return nil
}
func (f *fakeDirStore) Delete(context.Context, string) error { return nil }

func TestAddDirectoryStoresForOperator(t *testing.T) {
	store := &fakeDirStore{}
	s := &rosterConnect{deps: &Deps{DirStore: store}}

	_, err := s.AddDirectory(operatorCtx(), connect.NewRequest(&rosterv1.AddDirectoryRequest{
		Name: "acme", Domains: []string{" acme.example ", ""}, Endpoint: "http://ggs",
	}))
	if err != nil {
		t.Fatalf("AddDirectory: %v", err)
	}

	if store.put.Name != "acme" || store.put.Endpoint != "http://ggs" ||
		len(store.put.Domains) != 1 || store.put.Domains[0] != "acme.example" {
		t.Fatalf("stored = %+v", store.put)
	}
}

func TestAddDirectoryRejectsViewer(t *testing.T) {
	store := &fakeDirStore{}
	s := &rosterConnect{deps: &Deps{DirStore: store}}

	ctx := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleViewer})

	_, err := s.AddDirectory(ctx, connect.NewRequest(&rosterv1.AddDirectoryRequest{
		Name: "acme", Domains: []string{"acme.example"}, Endpoint: "http://ggs",
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer AddDirectory code = %v, want PermissionDenied", connect.CodeOf(err))
	}

	if store.puts != 0 {
		t.Errorf("Put called %d times for a viewer, want 0", store.puts)
	}
}

func TestSetPausedNeedsBroker(t *testing.T) {
	s := &rosterConnect{deps: &Deps{}} // no broker configured

	_, err := s.SetPaused(operatorCtx(), connect.NewRequest(&rosterv1.SetPausedRequest{Org: "acme", Paused: true}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("SetPaused without broker code = %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestSetReconcileEnabledGate(t *testing.T) {
	s := &rosterConnect{deps: &Deps{}}

	// Viewer is refused before the broker is even consulted.
	viewer := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleViewer})
	if _, err := s.SetReconcileEnabled(viewer, connect.NewRequest(&rosterv1.SetReconcileEnabledRequest{Org: "acme", Enabled: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer SetReconcileEnabled code = %v, want PermissionDenied", connect.CodeOf(err))
	}

	// Operator with no broker configured gets Unavailable.
	if _, err := s.SetReconcileEnabled(operatorCtx(), connect.NewRequest(&rosterv1.SetReconcileEnabledRequest{Org: "acme", Enabled: true})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("SetReconcileEnabled without broker code = %v, want Unavailable", connect.CodeOf(err))
	}
}
