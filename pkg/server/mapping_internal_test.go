package server

import "testing"

// The suggestion abbreviation follows the fleet's emp-{slug5} namespace
// convention: first initial + first four letters of the last name.
func TestConventionalAbbrevFollowsSlug5(t *testing.T) {
	cases := map[string]string{
		"Oleg Tsarev":             "otsar",
		"Konstantin Tereschenkov": "ktere",
		"Aleksandr Prokunin":      "aprok",
		"Li Bo":                   "lbo",     // short last names stay short
		"Madonna":                 "madonna", // single-word names pass through
		"Anne-Marie van der Berg": "aberg",
		"":                        "",
	}

	for name, want := range cases {
		if got := conventionalAbbrev(name); got != want {
			t.Errorf("conventionalAbbrev(%q) = %q, want %q", name, got, want)
		}
	}
}
