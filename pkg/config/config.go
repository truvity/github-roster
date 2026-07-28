// Package config is the service's whole configuration surface: one YAML
// document, validated at startup, never reloaded.
//
// The split between this file and the environment is deliberate. The
// document says *what* this instance manages — organizations, directory
// sources, teams — and is safe to keep in a ConfigMap and in version
// control. Secrets (the OIDC client secret, the session key) arrive through
// the environment and never appear here.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the parsed configuration document.
type Config struct {
	// Listen is the address of the web tier ("host:port"; ":8080").
	Listen string `yaml:"listen"`
	// HealthListen is the address of the health/metrics server, deliberately
	// a different port so the health check is not routable from outside.
	HealthListen string `yaml:"healthListen"`

	LogLevel  string `yaml:"logLevel"`
	LogFormat string `yaml:"logFormat"`

	OIDC    OIDC     `yaml:"oidc"`
	Sources []Source `yaml:"sources"`
	Orgs    []Org    `yaml:"orgs"`

	Mapping  Mapping  `yaml:"mapping"`
	Audit    Audit    `yaml:"audit"`
	Schedule Schedule `yaml:"schedule"`
}

// OIDC configures how a forwarded token becomes a role.
//
// The sign-in itself is not this service's business. A gateway in front of
// it runs the authorization code flow, holds the session cookie, validates
// the token against the issuer's JWKS and denies anyone outside the
// console's groups. What remains here is the claim-to-role mapping — the
// distinction between viewer and operator, which the gateway cannot make on
// this service's behalf because it drives what the pages render.
type OIDC struct {
	// Disabled skips token verification entirely, treating every request as
	// an operator. It exists for local development and tests. It must be
	// set explicitly: an empty issuer is a configuration mistake, and
	// mistaking one for "no auth wanted" would silently open an operator
	// console.
	Disabled bool `yaml:"disabled"`

	// Issuer is the identity provider, used to discover the JWKS the
	// forwarded token is verified against.
	Issuer string `yaml:"issuer"`

	// Audience, when set, is required to appear in the token's `aud`.
	// Empty accepts any token this issuer minted — which is the sensible
	// default behind a gateway that has already checked the audience, and
	// avoids naming the wrong value on providers whose access tokens carry
	// a project rather than a client.
	Audience string `yaml:"audience"`

	// RolesClaim names the claim holding the caller's group or role
	// memberships (an array of strings, or a single string).
	RolesClaim string `yaml:"rolesClaim"`
	// Roles maps this service's two roles onto values of RolesClaim.
	Roles Roles `yaml:"roles"`
}

// Roles are the only two roles the service knows.
//
// viewer sees structure. operator additionally sees the audit log, edits the
// mapping and triggers a sync. There is no third level: everything an
// operator can do is either reviewable in a dry run or recorded in the audit
// trail, and a finer grid would suggest a separation this service does not
// actually enforce.
type Roles struct {
	Viewer   string `yaml:"viewer"`
	Operator string `yaml:"operator"`
}

// Source is one corporate directory: who exists, who is suspended, and who
// is in which group.
type Source struct {
	Name string `yaml:"name"`
	// SSMPrefix holds the directory credentials — a service-account key and
	// the admin subject to impersonate.
	SSMPrefix string `yaml:"ssmPrefix"`
	// Domains restricts the source to these email domains. A directory may
	// serve several domains while this instance is only responsible for
	// some of them, and reading the rest would import people the service
	// has no business managing.
	Domains []string `yaml:"domains"`
}

// Org is one GitHub organization under management.
type Org struct {
	Name string `yaml:"name"`

	// ConsoleAppSSM holds the read-only App credentials used by the web
	// tier. ApplierAppSSM holds the write-capable App credentials, which
	// are only ever mounted into a reconciler Job — never read by the web
	// process. Keeping them apart here mirrors the runtime boundary.
	ConsoleAppSSM string `yaml:"consoleAppSSM"`
	ApplierAppSSM string `yaml:"applierAppSSM"`

	// Exceptions are logins the service never touches in either direction:
	// Apps, bots, and anything else whose membership is not a person's.
	Exceptions []string `yaml:"exceptions"`

	// Teams whose membership this service reconciles. Creating and deleting
	// teams is not this service's business — see docs/architecture.
	Teams map[string]Team `yaml:"teams"`
}

// Team is either directory-mapped or pinned, never both.
type Team struct {
	// Groups are directory groups whose union is the team's membership.
	// Read flat: a nested group is not expanded, because a team whose
	// membership you cannot determine by looking is not reviewable.
	Groups []string `yaml:"groups"`

	// Pinned teams are edited only in the operator UI and stored with the
	// mapping. Scheduled runs never touch them.
	Pinned bool `yaml:"pinned"`
}

// Mapping locates the person → handle store.
type Mapping struct {
	// SSMPrefix is the root of the parameter tree, e.g. "/roster/".
	SSMPrefix string `yaml:"ssmPrefix"`
}

// Audit locates the durable record of every run.
type Audit struct {
	Bucket string `yaml:"bucket"`
	// PrefixPerOrg files each record under its organization, which is what
	// lets one bucket policy per organization exist later.
	PrefixPerOrg bool `yaml:"prefixPerOrg"`
}

// Schedule controls the unattended, removals-only runs.
type Schedule struct {
	// RemovalsInterval is the service's contribution to the revocation SLA:
	// a leaver loses organization membership within one interval of being
	// suspended in the directory.
	RemovalsInterval time.Duration `yaml:"removalsInterval"`

	// MaxRemovalFraction stops a run that would remove an implausible share
	// of an organization — the guard against a directory returning nonsense
	// convincingly. 0 disables the guard.
	MaxRemovalFraction float64 `yaml:"maxRemovalFraction"`
}

// Defaults returns the configuration before the document is applied.
func Defaults() Config {
	return Config{
		Listen:       ":8080",
		HealthListen: ":7070",
		LogLevel:     "info",
		LogFormat:    "json",
		OIDC:         OIDC{RolesClaim: "groups"},
		Mapping:      Mapping{SSMPrefix: "/roster/"},
		Audit:        Audit{PrefixPerOrg: true},
		Schedule: Schedule{
			RemovalsInterval:   time.Hour,
			MaxRemovalFraction: 0.5,
		},
	}
}

// Load reads, parses and validates the document at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator-controlled (a ConfigMap mount), the same trust level as the document's contents
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	return Parse(data)
}

// Parse parses and validates a configuration document.
func Parse(data []byte) (*Config, error) {
	cfg := Defaults()

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // a typo'd key is a configuration bug, not something to ignore

	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// dns1123 is the shape a team name and a Kubernetes abbreviation must have.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate reports every way the document is unusable. It is strict on
// purpose: this service acts on people's access, and a configuration that is
// almost right is the most dangerous kind.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen is required")
	}

	if err := c.OIDC.validate(); err != nil {
		return err
	}

	if err := c.validateSources(); err != nil {
		return err
	}

	if err := c.validateOrgs(); err != nil {
		return err
	}

	if !strings.HasPrefix(c.Mapping.SSMPrefix, "/") || !strings.HasSuffix(c.Mapping.SSMPrefix, "/") {
		return fmt.Errorf("mapping.ssmPrefix %q must start and end with %q", c.Mapping.SSMPrefix, "/")
	}

	if c.Schedule.RemovalsInterval <= 0 {
		return fmt.Errorf("schedule.removalsInterval must be positive, got %s", c.Schedule.RemovalsInterval)
	}

	if c.Schedule.MaxRemovalFraction < 0 || c.Schedule.MaxRemovalFraction > 1 {
		return fmt.Errorf("schedule.maxRemovalFraction must be within [0,1], got %v", c.Schedule.MaxRemovalFraction)
	}

	return nil
}

func (o *OIDC) validate() error {
	if o.Disabled {
		return nil
	}

	for field, value := range map[string]string{
		"oidc.issuer":     o.Issuer,
		"oidc.rolesClaim": o.RolesClaim,
	} {
		if value == "" {
			return fmt.Errorf("%s is required (set oidc.disabled to run without authentication)", field)
		}
	}

	if o.Roles.Operator == "" {
		return fmt.Errorf("oidc.roles.operator is required: without it nobody could ever sync or edit the mapping")
	}

	if o.Roles.Viewer == o.Roles.Operator {
		return fmt.Errorf("oidc.roles.viewer and oidc.roles.operator are both %q, which would grant every viewer write access", o.Roles.Viewer)
	}

	return nil
}

func (c *Config) validateSources() error {
	seen := make(map[string]bool, len(c.Sources))

	for i := range c.Sources {
		s := &c.Sources[i]

		switch {
		case s.Name == "":
			return fmt.Errorf("sources[%d].name is required", i)
		case seen[s.Name]:
			return fmt.Errorf("sources[%d]: duplicate source name %q", i, s.Name)
		case s.SSMPrefix == "":
			return fmt.Errorf("sources[%q].ssmPrefix is required", s.Name)
		case len(s.Domains) == 0:
			// An unfiltered directory read imports every domain the
			// directory serves, including ones this instance does not
			// manage. Make the operator say which.
			return fmt.Errorf("sources[%q].domains is required: name the domains this source is responsible for", s.Name)
		}

		seen[s.Name] = true
	}

	return nil
}

func (c *Config) validateOrgs() error {
	if len(c.Orgs) == 0 {
		return fmt.Errorf("orgs is required: at least one organization must be under management")
	}

	seen := make(map[string]bool, len(c.Orgs))

	for i := range c.Orgs {
		o := &c.Orgs[i]

		switch {
		case o.Name == "":
			return fmt.Errorf("orgs[%d].name is required", i)
		case seen[o.Name]:
			return fmt.Errorf("orgs[%d]: duplicate organization %q", i, o.Name)
		case o.ConsoleAppSSM == "":
			return fmt.Errorf("orgs[%q].consoleAppSSM is required", o.Name)
		case o.ApplierAppSSM == "":
			return fmt.Errorf("orgs[%q].applierAppSSM is required", o.Name)
		case o.ConsoleAppSSM == o.ApplierAppSSM:
			// The whole security argument for this service is that the web
			// tier cannot write. One prefix for both credentials silently
			// dissolves that boundary.
			return fmt.Errorf("orgs[%q]: consoleAppSSM and applierAppSSM are the same path %q — the read-only and write credentials must be different Apps", o.Name, o.ConsoleAppSSM)
		}

		if err := validateTeams(o); err != nil {
			return err
		}

		seen[o.Name] = true
	}

	return nil
}

func validateTeams(o *Org) error {
	for name, team := range o.Teams {
		switch {
		case !dns1123.MatchString(name):
			return fmt.Errorf("orgs[%q].teams[%q]: team name must be lowercase alphanumeric with dashes", o.Name, name)
		case team.Pinned && len(team.Groups) > 0:
			return fmt.Errorf("orgs[%q].teams[%q]: a team is either pinned or directory-mapped, not both", o.Name, name)
		case !team.Pinned && len(team.Groups) == 0:
			return fmt.Errorf("orgs[%q].teams[%q]: needs groups, or pinned: true", o.Name, name)
		}
	}

	return nil
}

// Org returns the configuration for the named organization.
func (c *Config) Org(name string) (*Org, bool) {
	for i := range c.Orgs {
		if c.Orgs[i].Name == name {
			return &c.Orgs[i], true
		}
	}

	return nil, false
}

// IsException reports whether a login is exempt from all reconciliation.
func (o *Org) IsException(login string) bool {
	for _, e := range o.Exceptions {
		if strings.EqualFold(e, login) {
			return true
		}
	}

	return false
}
