package broker

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

type stubSource struct{ name string }

func (s stubSource) Name() string { return s.name }
func (s stubSource) Fetch(context.Context) (*directory.Snapshot, error) {
	return &directory.Snapshot{Source: s.name}, nil
}

func TestEffectiveConfigAndReload(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	git := &config.Config{Sources: []config.Source{{Name: "acme", Domains: []config.Domain{{Name: "acme.example"}}}}}

	d := &Deps{
		Logger:      log,
		Config:      git,
		Directories: directory.NewSet(log, stubSource{"acme"}),
		GitSources:  []directory.Source{stubSource{"acme"}},
		DirStore: fakeDirStore{dirs: []config.Source{
			{Name: "beta", Domains: []config.Domain{{Name: "beta.example"}}, Endpoint: "http://ggs-beta"},
		}},
	}

	// Before a reload, effectiveConfig is the git config.
	if got := d.effectiveConfig(); len(got.Sources) != 1 {
		t.Fatalf("pre-reload effective sources = %d, want 1", len(got.Sources))
	}

	d.reloadDirectories(context.Background())

	// After: the store directory is merged into effective Sources — so the
	// removals fail-safe (expected-sources reads Sources) sees it too — and
	// the git config object is untouched.
	eff := d.effectiveConfig()
	if len(eff.Sources) != 2 {
		t.Fatalf("post-reload effective sources = %d, want 2 (acme+beta)", len(eff.Sources))
	}
	if len(git.Sources) != 1 {
		t.Fatal("git config Sources must not be mutated")
	}

	// The Set now carries beta alongside acme.
	names := map[string]bool{}
	for _, st := range d.Directories.Statuses() {
		names[st.Source] = true
	}
	if !names["acme"] || !names["beta"] {
		t.Fatalf("Set after reload = %v, want acme+beta", names)
	}
}
