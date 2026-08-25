// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/orgstate"
)

type fakeDirStore struct{ dirs []config.Source }

func (f fakeDirStore) List(context.Context) ([]config.Source, error) { return f.dirs, nil }
func (f fakeDirStore) Put(context.Context, config.Source) error      { return nil }
func (f fakeDirStore) Delete(context.Context, string) error          { return nil }

type stubSource struct{ name string }

func (s stubSource) Name() string { return s.name }
func (s stubSource) Fetch(context.Context) (*directory.Snapshot, error) {
	return &directory.Snapshot{Source: s.name}, nil
}

// TestReloadDirectories is the console counterpart to the broker's
// TestEffectiveConfigAndReload: an operator-added store directory must fold
// into the live directory Set (git ∪ store) without a restart, git wins by
// name, and the git config object is never mutated.
func TestReloadDirectories(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	git := &config.Config{Sources: []config.Source{{Name: "acme", Domains: []config.Domain{{Name: "acme.example"}}}}}

	l := &readLayers{
		cfg:         git,
		gitSources:  []directory.Source{stubSource{"acme"}},
		Directories: directory.NewSet(log, stubSource{"acme"}),
		DirStore: fakeDirStore{dirs: []config.Source{
			// A fresh store directory (resolver-backed) …
			{Name: "beta", Domains: []config.Domain{{Name: "beta.example"}}, Endpoint: "http://ggs-beta"},
			// … and one that collides with a git source by name: git wins,
			// so this must NOT replace acme (which keeps its live client).
			{Name: "acme", Domains: []config.Domain{{Name: "acme.example"}}, Endpoint: "http://ggs-acme"},
		}},
	}

	l.reloadDirectories(context.Background(), log)

	names := map[string]bool{}
	for _, st := range l.Directories.Statuses() {
		names[st.Source] = true
	}

	if !names["acme"] || !names["beta"] {
		t.Fatalf("Set after reload = %v, want acme+beta", names)
	}

	if len(names) != 2 {
		t.Fatalf("Set after reload has %d sources, want 2 (git acme wins over the store acme)", len(names))
	}

	if len(git.Sources) != 1 {
		t.Fatal("git config Sources must not be mutated by a reload")
	}
}

// stubPart is a minimal orgstate.PartReader recording ReadMembership calls, so
// a test can tell which cache a reloadableOrg is currently delegating to.
type stubPart struct{ reads int }

func (s *stubPart) ReadMembership(context.Context) (*orgstate.Membership, error) {
	s.reads++

	return &orgstate.Membership{}, nil
}

func (s *stubPart) ReadTeams(context.Context) (*orgstate.TeamState, error) {
	return &orgstate.TeamState{}, nil
}

// TestReloadableOrgSwap: the wrapper swaps its reader only when the credentials
// fingerprint changes (preserving the warm cache otherwise), and Read/Invalidate
// always delegate to the current reader.
func TestReloadableOrgSwap(t *testing.T) {
	p1 := &stubPart{}
	ro := &reloadableOrg{name: "acme", fp: "fp1"}
	ro.current.Store(orgstate.NewCache("acme", p1))

	// Same fingerprint: no swap, and Read still hits p1.
	if ro.swapIfChanged(orgstate.NewCache("acme", &stubPart{}), "fp1") {
		t.Fatal("swapIfChanged returned true for an unchanged fingerprint")
	}

	if _, err := ro.Read(context.Background()); err != nil {
		t.Fatalf("read: %v", err)
	}

	if p1.reads == 0 {
		t.Fatal("Read did not delegate to the current (p1) cache")
	}

	// Changed fingerprint: swap to p2, and Read now hits p2.
	p2 := &stubPart{}
	if !ro.swapIfChanged(orgstate.NewCache("acme", p2), "fp2") {
		t.Fatal("swapIfChanged returned false for a changed fingerprint")
	}

	if _, err := ro.Read(context.Background()); err != nil {
		t.Fatalf("read after swap: %v", err)
	}

	if p2.reads == 0 {
		t.Fatal("Read did not delegate to the swapped-in (p2) cache")
	}
}

// TestCredFingerprint: stable, sensitive to a rotated key, and unambiguous
// across field boundaries (the unit separator).
func TestCredFingerprint(t *testing.T) {
	first := credFingerprint("1", "2", "key")
	if again := credFingerprint("1", "2", "key"); first != again {
		t.Fatal("fingerprint is not stable")
	}

	if credFingerprint("1", "2", "key") == credFingerprint("1", "2", "rotated") {
		t.Fatal("fingerprint collides across a rotated private key")
	}

	if credFingerprint("12", "", "x") == credFingerprint("1", "2x", "") {
		t.Fatal("fingerprint is ambiguous across field boundaries")
	}
}
