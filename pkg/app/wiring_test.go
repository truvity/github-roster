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
	git := &config.Config{Sources: []config.Source{{Name: "acme", Domains: []string{"acme.example"}}}}

	l := &readLayers{
		cfg:         git,
		gitSources:  []directory.Source{stubSource{"acme"}},
		Directories: directory.NewSet(log, stubSource{"acme"}),
		DirStore: fakeDirStore{dirs: []config.Source{
			// A fresh store directory (resolver-backed) …
			{Name: "beta", Domains: []string{"beta.example"}, Endpoint: "http://ggs-beta"},
			// … and one that collides with a git source by name: git wins,
			// so this must NOT replace acme (which keeps its live client).
			{Name: "acme", Domains: []string{"acme.example"}, Endpoint: "http://ggs-acme"},
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
