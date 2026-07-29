package mapping_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/mapping"
)

func entry(name, github, k8s string) mapping.Entry {
	return mapping.Entry{Name: name, GitHub: github, K8s: k8s, Class: mapping.ClassEmployee}
}

func TestValidateEntry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		entry   mapping.Entry
		wantErr string
	}{
		{"valid", entry("A Person", "octocat", "aperson"), ""},
		{"abbreviation is optional", mapping.Entry{Name: "A Bot", GitHub: "bot", Class: mapping.ClassBot}, ""},
		{"no name", entry("", "octocat", "x"), "name is required"},
		// Two entries that look identical and join differently is the worst
		// kind of duplicate.
		{"padded name", entry(" A Person ", "octocat", "x"), "leading or trailing whitespace"},
		{"no class", mapping.Entry{Name: "A Person", GitHub: "octocat"}, "class"},
		{"unknown class", mapping.Entry{Name: "A Person", GitHub: "octocat", Class: "contractor"}, "class"},
		{"no github login", entry("A Person", "", "x"), "github login is required"},
		{"github login with a space", entry("A Person", "oct cat", "x"), "not a valid GitHub username"},
		{"github login with double hyphen", entry("A Person", "oct--cat", "x"), "not a valid GitHub username"},
		{"github login starting with hyphen", entry("A Person", "-octocat", "x"), "not a valid GitHub username"},
		{"uppercase abbreviation", entry("A Person", "octocat", "APerson"), "DNS-1123"},
		{"abbreviation with underscore", entry("A Person", "octocat", "a_person"), "DNS-1123"},
		{"overlong abbreviation", entry("A Person", "octocat", strings.Repeat("a", 21)), "longer than 20"},
		{
			name:    "pinned team without an org",
			entry:   mapping.Entry{Name: "A Bot", GitHub: "bot", Class: mapping.ClassBot, Pinned: []string{"robots"}},
			wantErr: "<org>/<team>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := mapping.ValidateEntry(tc.entry)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestCheckInvariants(t *testing.T) {
	t.Parallel()

	existing := []mapping.Entry{
		entry("A Person", "octocat", "aperson"),
		entry("B Person", "hubot", "bperson"),
	}

	t.Run("a fresh entry is fine", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, mapping.CheckInvariants(entry("C Person", "monalisa", "cperson"), mapping.Entry{}, existing))
	})

	// Changing an abbreviation orphans a live namespace whose name nothing
	// else will ever update.
	t.Run("abbreviation is immutable once assigned", func(t *testing.T) {
		t.Parallel()

		err := mapping.CheckInvariants(entry("A Person", "octocat", "renamed"), existing[0], existing)
		require.ErrorIs(t, err, mapping.ErrImmutable)
	})

	// But filling in a blank one is an ordinary edit.
	t.Run("assigning a first abbreviation is allowed", func(t *testing.T) {
		t.Parallel()

		blank := mapping.Entry{Name: "D Person", GitHub: "dperson", Class: mapping.ClassEmployee}
		require.NoError(t, mapping.CheckInvariants(entry("D Person", "dperson", "dperson"), blank, existing))
	})

	t.Run("abbreviation must be unique", func(t *testing.T) {
		t.Parallel()

		err := mapping.CheckInvariants(entry("C Person", "monalisa", "aperson"), mapping.Entry{}, existing)
		require.ErrorIs(t, err, mapping.ErrDuplicate)
		require.ErrorContains(t, err, "A Person")
	})

	// GitHub logins are case-insensitive; two people mapped to one account
	// makes the join ambiguous in a way nothing downstream catches.
	t.Run("github login must be unique, case-insensitively", func(t *testing.T) {
		t.Parallel()

		err := mapping.CheckInvariants(entry("C Person", "OctoCat", "cperson"), mapping.Entry{}, existing)
		require.ErrorIs(t, err, mapping.ErrDuplicate)
	})

	t.Run("an entry does not collide with itself", func(t *testing.T) {
		t.Parallel()

		updated := existing[0]
		updated.Class = mapping.ClassBot
		require.NoError(t, mapping.CheckInvariants(updated, existing[0], existing))
	})
}

func TestSlug(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Oleg Tsarev":   "oleg-tsarev",
		"A  Person":     "a-person",
		"Ann-Marie Doe": "ann-marie-doe",
		"O'Brien Sean":  "o-brien-sean",
		"  Padded  ":    "padded",
		"MiXeD CaSe":    "mixed-case",
	}

	for in, want := range cases {
		require.Equal(t, want, mapping.Slug(in), "Slug(%q)", in)
	}
}
