package configstore

import "testing"

func TestSourcesFromSkipsIncomplete(t *testing.T) {
	byName := map[string]map[string]string{
		"good":        {"endpoint": "http://ggs", "domains": "a.example, b.example", "probeGroup": "all@a.example"},
		"no-domains":  {"endpoint": "http://ggs"},
		"no-endpoint": {"domains": "c.example"},
	}

	got := sourcesFrom(byName)
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("only the complete directory should survive, got %+v", got)
	}
	if len(got[0].Domains) != 2 || got[0].Domains[0] != "a.example" {
		t.Fatalf("domains not parsed/trimmed: %+v", got[0].Domains)
	}
	if got[0].Endpoint != "http://ggs" || got[0].ProbeGroup != "all@a.example" {
		t.Fatalf("fields not mapped: %+v", got[0])
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList("  a , ,b ,"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitList trim/skip failed: %v", got)
	}
	if splitList("   ") != nil {
		t.Fatal("blank must be nil")
	}
}

func TestSplitParam(t *testing.T) {
	s := &SSM{prefix: "/roster/directories/"}
	name, field, ok := s.splitParam("/roster/directories/acme/endpoint")
	if !ok || name != "acme" || field != "endpoint" {
		t.Fatalf("splitParam = %q %q %v", name, field, ok)
	}
	if _, _, ok := s.splitParam("/other/thing"); ok {
		t.Fatal("foreign prefix must not parse")
	}
}
