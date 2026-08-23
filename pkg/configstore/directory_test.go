package configstore_test

import (
	"testing"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/configstore"
)

func TestMergeDirectories(t *testing.T) {
	iac := []config.Source{{Name: "acme", Domains: []string{"acme.example"}}}
	store := []config.Source{
		{Name: "acme", Domains: []string{"store-should-lose"}, Endpoint: "http://x"},
		{Name: "beta", Domains: []string{"beta.example"}, Endpoint: "http://b"},
	}

	got := configstore.MergeDirectories(iac, store)
	if len(got) != 2 {
		t.Fatalf("want 2 (acme from git, beta from store), got %d: %+v", len(got), got)
	}

	byName := map[string]config.Source{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if byName["acme"].Endpoint != "" {
		t.Fatalf("git acme must win (no store endpoint), got %+v", byName["acme"])
	}
	if byName["beta"].Endpoint != "http://b" {
		t.Fatalf("store-only beta must be appended, got %+v", byName["beta"])
	}
}
