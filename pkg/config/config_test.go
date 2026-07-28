package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/truvity/github-roster/pkg/config"
)

// minimal is the smallest document that validates. Every negative case below
// is this document with exactly one thing wrong, so a failure names the rule
// that fired rather than a pile of unrelated errors.
const minimal = `
oidc:
  issuer: https://issuer.example
  roles:
    viewer: viewers@example.com
    operator: operators@example.com
orgs:
  - name: example
    consoleAppSSM: /secrets/roster/console/example
    applierAppSSM: /secrets/roster/applier/example
    teams:
      engineers:
        groups: [engineers@example.com]
      robots:
        pinned: true
audit:
  bucket: example-roster-audit
`

func TestParseMinimalAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":8080")
	}

	if cfg.HealthListen != ":7070" {
		t.Errorf("HealthListen = %q, want %q", cfg.HealthListen, ":7070")
	}

	if cfg.Mapping.SSMPrefix != "/roster/" {
		t.Errorf("Mapping.SSMPrefix = %q, want %q", cfg.Mapping.SSMPrefix, "/roster/")
	}

	if cfg.Schedule.RemovalsInterval != time.Hour {
		t.Errorf("RemovalsInterval = %s, want 1h", cfg.Schedule.RemovalsInterval)
	}

	if cfg.OIDC.RolesClaim != "groups" {
		t.Errorf("RolesClaim = %q, want %q", cfg.OIDC.RolesClaim, "groups")
	}

	if !cfg.Audit.PrefixPerOrg {
		t.Error("Audit.PrefixPerOrg defaulted to false; per-org prefixes are the default")
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		doc      string
		contains string
	}{
		{
			name:     "unknown key",
			doc:      minimal + "\nnosuchkey: 1\n",
			contains: "nosuchkey",
		},
		{
			name: "no orgs",
			doc: `
oidc: {disabled: true}
orgs: []
`,
			contains: "at least one organization",
		},
		{
			// The read/write split is the service's central security claim.
			// Sharing one credential path would quietly retract it.
			name: "console and applier share a credential path",
			doc: strings.Replace(minimal,
				"applierAppSSM: /secrets/roster/applier/example",
				"applierAppSSM: /secrets/roster/console/example", 1),
			contains: "must be different Apps",
		},
		{
			name: "team both pinned and directory-mapped",
			doc: strings.Replace(minimal,
				"      robots:\n        pinned: true",
				"      robots:\n        pinned: true\n        groups: [bots@example.com]", 1),
			contains: "either pinned or directory-mapped",
		},
		{
			name: "team neither pinned nor directory-mapped",
			doc: strings.Replace(minimal,
				"      robots:\n        pinned: true",
				"      robots: {}", 1),
			contains: "needs groups, or pinned",
		},
		{
			name: "team name is not DNS-1123",
			doc: strings.Replace(minimal,
				"      engineers:", "      Engineers_Team:", 1),
			contains: "lowercase alphanumeric",
		},
		{
			// An empty issuer must not be read as "no authentication
			// wanted" — that would open an operator console silently.
			name:     "missing issuer without an explicit opt-out",
			doc:      strings.Replace(minimal, "  issuer: https://issuer.example\n", "", 1),
			contains: "oidc.issuer is required",
		},
		{
			name: "viewer and operator claims collide",
			doc: strings.Replace(minimal,
				"    viewer: viewers@example.com",
				"    viewer: operators@example.com", 1),
			contains: "grant every viewer write access",
		},
		{
			name: "source without domains",
			doc: minimal + `
sources:
  - name: corp
    ssmPrefix: /secrets/directory/corp
`,
			contains: "domains is required",
		},
		{
			name: "duplicate source names",
			doc: minimal + `
sources:
  - name: corp
    ssmPrefix: /secrets/directory/corp
    domains: [example.com]
  - name: corp
    ssmPrefix: /secrets/directory/other
    domains: [other.example]
`,
			contains: "duplicate source name",
		},
		{
			name:     "mapping prefix without a trailing slash",
			doc:      minimal + "\nmapping: {ssmPrefix: /roster}\n",
			contains: "must start and end with",
		},
		{
			name:     "non-positive removals interval",
			doc:      minimal + "\nschedule: {removalsInterval: 0s}\n",
			contains: "removalsInterval must be positive",
		},
		{
			name:     "removal fraction out of range",
			doc:      minimal + "\nschedule: {maxRemovalFraction: 1.5}\n",
			contains: "within [0,1]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse([]byte(tc.doc))
			if err == nil {
				t.Fatalf("Parse() succeeded; want an error containing %q", tc.contains)
			}

			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("Parse() = %v, want an error containing %q", err, tc.contains)
			}
		})
	}
}

func TestDisabledOIDCNeedsNothingElse(t *testing.T) {
	t.Parallel()

	doc := `
oidc: {disabled: true}
orgs:
  - name: example
    consoleAppSSM: /secrets/roster/console/example
    applierAppSSM: /secrets/roster/applier/example
`

	if _, err := config.Parse([]byte(doc)); err != nil {
		t.Fatalf("Parse() = %v", err)
	}
}

func TestExceptionsAreCaseInsensitive(t *testing.T) {
	t.Parallel()

	// GitHub logins are case-insensitive, and an exception list that missed
	// a bot because of capitalization would remove the bot.
	doc := strings.Replace(minimal,
		"    consoleAppSSM:",
		"    exceptions: [Example-Bot]\n    consoleAppSSM:", 1)

	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	org, ok := cfg.Org("example")
	if !ok {
		t.Fatal("Org(\"example\") not found")
	}

	for _, login := range []string{"Example-Bot", "example-bot", "EXAMPLE-BOT"} {
		if !org.IsException(login) {
			t.Errorf("IsException(%q) = false, want true", login)
		}
	}

	if org.IsException("someone-else") {
		t.Error("IsException(\"someone-else\") = true, want false")
	}
}
