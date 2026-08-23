package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
companies:
  example:
    directory:
      ssmPrefix: /secrets/google-workspace/example
      domains: [example.com]
    github:
      org: example
      consoleAppSSM: /secrets/roster/console/example
      applierAppSSM: /secrets/roster/applier/example
      teams:
        team-engineers:
          groups: [team-engineers@example.com]
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
			name: "no companies",
			doc: `
oidc: {disabled: true}
companies: {}
`,
			contains: "at least one company",
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
				"        robots:\n          pinned: true",
				"        robots:\n          pinned: true\n          groups: [bots@example.com]", 1),
			contains: "either pinned or directory-mapped",
		},
		{
			name: "team neither pinned nor directory-mapped",
			doc: strings.Replace(minimal,
				"        robots:\n          pinned: true",
				"        robots: {}", 1),
			contains: "needs groups and/or members, or pinned",
		},
		{
			name: "team name is not DNS-1123",
			doc: strings.Replace(minimal,
				"        team-engineers:", "        Engineers_Team:", 1),
			contains: "lowercase alphanumeric",
		},
		{
			// The naming invariant: the name alone says whose directory
			// governs the team; a mismatched group cannot slip through.
			name: "directory-mapped team outside the naming families",
			doc: strings.Replace(minimal,
				"        team-engineers:", "        engineers:", 1),
			contains: "team-<x> or partner-<code>-team-<x>",
		},
		{
			name: "team group local part disagrees with the team name",
			doc: strings.Replace(minimal,
				"groups: [team-engineers@example.com]",
				"groups: [devs@example.com]", 1),
			contains: "local part and the team name are the same thing",
		},
		{
			name: "team group outside the owning company's domains",
			doc: strings.Replace(minimal,
				"groups: [team-engineers@example.com]",
				"groups: [team-engineers@elsewhere.example]", 1),
			contains: "outside the owning company's domains",
		},
		{
			name: "partner team names an unconfigured company",
			doc: strings.Replace(minimal,
				"        robots:\n          pinned: true",
				"        partner-nosuch-team-x:\n          groups: [team-x@nosuch.example]", 1),
			contains: "is not configured",
		},
		{
			name: "pinned team squatting on the directory-mapped family",
			doc: strings.Replace(minimal,
				"        robots:\n          pinned: true",
				"        team-bots:\n          pinned: true", 1),
			contains: "pinned teams need a name outside them",
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
			name: "company directory without domains",
			doc: strings.Replace(minimal,
				"      domains: [example.com]\n", "", 1),
			contains: "domains is required",
		},
		{
			name:     "company code outside the pattern",
			doc:      strings.Replace(minimal, "  example:", "  Example:", 1),
			contains: "company code must be lowercase",
		},
		{
			name:     "mapping prefix without a trailing slash",
			doc:      minimal + "\nmapping: {ssmPrefix: /roster}\n",
			contains: "must start and end with",
		},
		{
			name:     "negative removals interval",
			doc:      minimal + "\nschedule: {removalsInterval: -1s}\n",
			contains: "removalsInterval must be zero (disabled) or positive",
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
companies:
  example:
    directory:
      ssmPrefix: /secrets/google-workspace/example
      domains: [example.com]
    github:
      org: example
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
		"      consoleAppSSM:",
		"      exceptions: [Example-Bot]\n      consoleAppSSM:", 1)

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

func TestAuditPrefixMustEndWithSlash(t *testing.T) {
	t.Parallel()

	bad := strings.Replace(minimal,
		"audit:\n  bucket: example-roster-audit",
		"audit:\n  bucket: example-roster-audit\n  prefix: ci-truvity/r1", 1)
	_, err := config.Parse([]byte(bad))
	require.ErrorContains(t, err, "must end with")

	good := strings.Replace(minimal,
		"audit:\n  bucket: example-roster-audit",
		"audit:\n  bucket: example-roster-audit\n  prefix: ci-truvity/r1/", 1)
	cfg, err := config.Parse([]byte(good))
	require.NoError(t, err)
	require.Equal(t, "ci-truvity/r1/", cfg.Audit.Prefix)
}

func TestMappedGroupsForDomainsFiltersBySource(t *testing.T) {
	t.Parallel()

	doc := `
oidc: {disabled: true}
companies:
  corp:
    directory:
      ssmPrefix: /secrets/directory/corp
      domains: [example.com]
    github:
      org: example
      consoleAppSSM: /secrets/roster/console/example
      applierAppSSM: /secrets/roster/applier/example
      teams:
        team-devs:
          groups: [team-devs@example.com]
        partner-pt-team-ext:
          groups: [team-ext@partner.example]
  pt:
    directory:
      ssmPrefix: /secrets/directory/pt
      domains: [partner.example]
`

	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	// Each source is asked only about the groups its directory owns — a
	// foreign group would 403 and poison the whole fetch.
	if got := cfg.MappedGroupsForDomains([]string{"example.com"}); len(got) != 1 || got[0] != "team-devs@example.com" {
		t.Errorf("corp groups = %v, want only team-devs@example.com", got)
	}

	if got := cfg.MappedGroupsForDomains([]string{"partner.example"}); len(got) != 1 || got[0] != "team-ext@partner.example" {
		t.Errorf("pt groups = %v, want only team-ext@partner.example", got)
	}
}

func TestReconcileConfig(t *testing.T) {
	t.Parallel()

	t.Run("default interval is 15m", func(t *testing.T) {
		cfg, err := config.Parse([]byte(minimal))
		require.NoError(t, err)
		require.Equal(t, 15*time.Minute, cfg.Reconcile.Interval)
	})

	t.Run("per-org enablement is born false and derives onto Org", func(t *testing.T) {
		cfg, err := config.Parse([]byte(minimal))
		require.NoError(t, err)
		require.Len(t, cfg.Orgs, 1)
		require.False(t, cfg.Orgs[0].ReconcileEnabled, "org must be born disabled (day-0 gate)")
	})

	t.Run("reconcileEnabled derives onto Org", func(t *testing.T) {
		doc := strings.Replace(minimal,
			"      org: example\n",
			"      org: example\n      reconcileEnabled: true\n", 1)
		cfg, err := config.Parse([]byte(doc))
		require.NoError(t, err)
		require.Len(t, cfg.Orgs, 1)
		require.True(t, cfg.Orgs[0].ReconcileEnabled)
	})

	t.Run("negative interval is rejected", func(t *testing.T) {
		doc := minimal + "reconcile:\n  interval: -1s\n"
		_, err := config.Parse([]byte(doc))
		require.Error(t, err)
	})
}
