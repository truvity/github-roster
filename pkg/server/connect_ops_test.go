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

// fakeControl records the pause/enable writes the console makes.
type fakeControl struct {
	paused  map[string]bool
	enabled map[string]bool
}

func newFakeControl() *fakeControl {
	return &fakeControl{paused: map[string]bool{}, enabled: map[string]bool{}}
}

func (f *fakeControl) Paused(_ context.Context, org string) (bool, error) { return f.paused[org], nil }
func (f *fakeControl) SetPaused(_ context.Context, org string, paused bool) error {
	f.paused[org] = paused

	return nil
}

func (f *fakeControl) EnabledOverride(_ context.Context, org string) (*bool, error) {
	if v, ok := f.enabled[org]; ok {
		return &v, nil
	}

	return nil, nil
}
func (f *fakeControl) SetEnabled(_ context.Context, org string, enabled bool) error {
	f.enabled[org] = enabled

	return nil
}

// The control write is the console's own — it must NOT need the broker (whose
// role is read-only on SSM; routing the write through it was the AccessDenied
// regression this guards against).
func TestSetPausedWritesViaControlWithoutBroker(t *testing.T) {
	ctrl := newFakeControl()
	s := &rosterConnect{deps: &Deps{Control: ctrl}} // note: no Broker

	if _, err := s.SetPaused(operatorCtx(), connect.NewRequest(&rosterv1.SetPausedRequest{Org: "acme", Paused: true})); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}

	if !ctrl.paused["acme"] {
		t.Fatal("SetPaused did not write the flag via the control store")
	}
}

func TestSetPausedNeedsControl(t *testing.T) {
	s := &rosterConnect{deps: &Deps{}} // no control store

	_, err := s.SetPaused(operatorCtx(), connect.NewRequest(&rosterv1.SetPausedRequest{Org: "acme", Paused: true}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("SetPaused without control code = %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestSetReconcileEnabledWritesViaControlWithoutBroker(t *testing.T) {
	ctrl := newFakeControl()
	s := &rosterConnect{deps: &Deps{Control: ctrl}} // no Broker

	if _, err := s.SetReconcileEnabled(operatorCtx(), connect.NewRequest(&rosterv1.SetReconcileEnabledRequest{Org: "acme", Enabled: true})); err != nil {
		t.Fatalf("SetReconcileEnabled: %v", err)
	}

	if !ctrl.enabled["acme"] {
		t.Fatal("SetReconcileEnabled did not write the flag via the control store")
	}
}

func TestSetReconcileEnabledGate(t *testing.T) {
	s := &rosterConnect{deps: &Deps{Control: newFakeControl()}}

	// Viewer is refused before the control store is even consulted.
	viewer := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleViewer})
	if _, err := s.SetReconcileEnabled(viewer, connect.NewRequest(&rosterv1.SetReconcileEnabledRequest{Org: "acme", Enabled: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer SetReconcileEnabled code = %v, want PermissionDenied", connect.CodeOf(err))
	}

	// Operator with no control store configured gets Unavailable.
	bare := &rosterConnect{deps: &Deps{}}
	if _, err := bare.SetReconcileEnabled(operatorCtx(), connect.NewRequest(&rosterv1.SetReconcileEnabledRequest{Org: "acme", Enabled: true})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("SetReconcileEnabled without control code = %v, want Unavailable", connect.CodeOf(err))
	}
}
