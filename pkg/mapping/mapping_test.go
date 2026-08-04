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

// withEmails is the same entry plus the addresses the directories know the
// person under. Several per person is the normal case, not the exception.
func withEmails(e mapping.Entry, emails ...string) mapping.Entry {
	e.Emails = emails

	return e
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

// TestIdentityUniqueness pins the rule the generated per-person email set
// depends on: every identity a person has — each address, and their GitHub
// login — resolves to exactly one entry, and therefore to exactly one
// namespace. Several addresses per person is the normal case and stays
// legal; the same address on two people never is.
func TestIdentityUniqueness(t *testing.T) {
	t.Parallel()

	// "Legacy Person" carries a mixed-case address on purpose. ValidateEntry
	// has forced lowercase since it was written, but an entry stored before
	// that rule must still not let a duplicate through.
	stored := []mapping.Entry{
		withEmails(entry("A Person", "octocat", "aperson"), "a.person@example.com", "a.person@example.invalid"),
		withEmails(entry("B Person", "hubot", "bperson"), "b.person@example.com"),
		withEmails(entry("Legacy Person", "legacy", "legacy"), "Legacy.Person@Example.com"),
	}

	cases := []struct {
		name string
		next mapping.Entry
		// before is the entry being replaced; the zero Entry for a create.
		before mapping.Entry
		// wantIs is the sentinel the error must wrap, nil when the write is
		// allowed. wantErr is a substring of the message, checked whenever
		// it is set — usually the name of the entry already holding the
		// identity, which is what an operator needs to see.
		wantIs  error
		wantErr string
	}{
		{
			name: "several addresses on one person is normal",
			next: withEmails(entry("C Person", "monalisa", "cperson"),
				"c.person@example.com", "c.person@example.invalid", "c@example.test"),
		},
		{
			// Deliberately not an error. A person with no address simply
			// does not appear in what the fleet generates from this store,
			// which is a gap to surface at the moment it is created — not
			// a reason to refuse the write. Bots never have one at all.
			name: "no addresses at all is allowed",
			next: entry("C Person", "monalisa", "cperson"),
		},
		{
			name:    "an address already held by another entry",
			next:    withEmails(entry("C Person", "monalisa", "cperson"), "a.person@example.com"),
			wantIs:  mapping.ErrDuplicate,
			wantErr: "A Person",
		},
		{
			name: "a later address collides even when the first does not",
			next: withEmails(entry("C Person", "monalisa", "cperson"),
				"c.person@example.com", "b.person@example.com"),
			wantIs:  mapping.ErrDuplicate,
			wantErr: "B Person",
		},
		{
			name:    "an address stored in another case is the same address",
			next:    withEmails(entry("C Person", "monalisa", "cperson"), "legacy.person@example.com"),
			wantIs:  mapping.ErrDuplicate,
			wantErr: "Legacy Person",
		},
		{
			// The mixed-case duplicate cannot reach the uniqueness check
			// from this side: the single-entry rule rejects it first. Both
			// answers are a rejection; only the message differs.
			name:    "a mixed-case address is rejected before uniqueness",
			next:    withEmails(entry("C Person", "monalisa", "cperson"), "A.Person@example.com"),
			wantErr: "lowercase",
		},
		{
			name:    "a github login already held by another entry",
			next:    entry("C Person", "hubot", "cperson"),
			wantIs:  mapping.ErrDuplicate,
			wantErr: "B Person",
		},
		{
			// GitHub logins are case-insensitive, so these are one account.
			name:    "a github login differing only in case",
			next:    entry("C Person", "OctoCat", "cperson"),
			wantIs:  mapping.ErrDuplicate,
			wantErr: "A Person",
		},
		{
			name: "an entry keeps its own addresses across an edit",
			next: withEmails(entry("A Person", "octocat", "aperson"),
				"a.person@example.com", "a.person@example.invalid"),
			before: stored[0],
		},
		{
			name:   "an entry may drop one of its own addresses",
			next:   withEmails(entry("A Person", "octocat", "aperson"), "a.person@example.invalid"),
			before: stored[0],
		},
		{
			// Editing skips the entry against itself, and must not thereby
			// skip the entry it is stealing an address from.
			name: "an edit that adds another entry's address is still caught",
			next: withEmails(entry("A Person", "octocat", "aperson"),
				"a.person@example.com", "b.person@example.com"),
			before:  stored[0],
			wantIs:  mapping.ErrDuplicate,
			wantErr: "B Person",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := mapping.CheckInvariants(tc.next, tc.before, stored)

			if tc.wantIs == nil && tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			if tc.wantIs != nil {
				require.ErrorIs(t, err, tc.wantIs)
			}

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
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
