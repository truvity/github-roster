package mapping_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/truvity/github-roster/pkg/mapping"
)

// memStore is a tiny in-memory Store for overlay tests.
type memStore struct{ m map[string]mapping.Entry }

func newMem(entries ...mapping.Entry) *memStore {
	s := &memStore{m: map[string]mapping.Entry{}}
	for i := range entries {
		s.m[entries[i].Name] = entries[i]
	}
	return s
}
func (s *memStore) List(context.Context) ([]mapping.Entry, error) {
	out := make([]mapping.Entry, 0, len(s.m))
	for k := range s.m {
		out = append(out, s.m[k])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (s *memStore) Get(_ context.Context, name string) (mapping.Entry, error) {
	e, ok := s.m[name]
	if !ok {
		return mapping.Entry{}, mapping.ErrNotFound
	}
	return e, nil
}
func (s *memStore) Put(_ context.Context, e mapping.Entry) error { s.m[e.Name] = e; return nil }
func (s *memStore) Delete(_ context.Context, name string) error  { delete(s.m, name); return nil }

func ovEntry(name, login string) mapping.Entry {
	return mapping.Entry{Name: name, GitHub: login, Class: mapping.ClassEmployee}
}

func TestOverlayMergeAndShadow(t *testing.T) {
	inner := newMem(ovEntry("Dana Okafor", "dokafor"), ovEntry("Mikael Strand", "mstrand"))
	ov, err := mapping.NewOverlay(inner, []mapping.Entry{ovEntry("Mikael Strand", "mstrand-git"), ovEntry("Ada Lovelace", "alovelace")})
	if err != nil {
		t.Fatal(err)
	}

	list, err := ov.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range list {
		got[e.Name] = e.GitHub
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 merged entries, got %d: %v", len(got), got)
	}
	if got["Mikael Strand"] != "mstrand-git" {
		t.Fatalf("git entry must shadow the store: %v", got)
	}
	if got["Ada Lovelace"] != "alovelace" || got["Dana Okafor"] != "dokafor" {
		t.Fatalf("merge wrong: %v", got)
	}
}

func TestOverlayRefusesWritesToGitEntries(t *testing.T) {
	inner := newMem(ovEntry("Dana Okafor", "dokafor"))
	ov, err := mapping.NewOverlay(inner, []mapping.Entry{ovEntry("Ada Lovelace", "alovelace")})
	if err != nil {
		t.Fatal(err)
	}

	if err := ov.Put(context.Background(), ovEntry("Ada Lovelace", "x")); !errors.Is(err, mapping.ErrManagedInGit) {
		t.Fatalf("Put to a git entry must be ErrManagedInGit, got %v", err)
	}
	if err := ov.Delete(context.Background(), "Ada Lovelace"); !errors.Is(err, mapping.ErrManagedInGit) {
		t.Fatalf("Delete of a git entry must be ErrManagedInGit, got %v", err)
	}
	// A store-owned name still writes.
	if err := ov.Put(context.Background(), ovEntry("Dana Okafor", "dokafor2")); err != nil {
		t.Fatalf("Put to a store entry must succeed, got %v", err)
	}
}
