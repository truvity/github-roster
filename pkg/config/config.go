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
	"sort"
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

	OIDC OIDC `yaml:"oidc"`

	// Companies is the configuration's spine (identity-model.md): each
	// company owns one directory and at most one GitHub organization.
	// Cross-company access is expressed as partner-<code>-team-<x> teams
	// whose groups live in company <code>'s directory.
	Companies map[string]Company `yaml:"companies"`

	// Sources and Orgs are DERIVED from Companies at load time (sorted
	// by company code) so the rest of the service keeps its shape; they
	// are not part of the document.
	Sources []Source `yaml:"-"`
	Orgs    []Org    `yaml:"-"`

	Mapping    Mapping    `yaml:"mapping"`
	Audit      Audit      `yaml:"audit"`
	Schedule   Schedule   `yaml:"schedule"`
	Reconciler Reconciler `yaml:"reconciler"`
	Reconcile  Reconcile  `yaml:"reconcile"`
	Broker     Broker     `yaml:"broker"`
}

// Broker configures the console's connection to the applier broker — the
// service holding the write credential behind an intent-only API.
type Broker struct {
	// URL is the broker's base URL. Empty means no broker: the sync
	// surface reports itself unavailable.
	URL string `yaml:"url"`
}

// Reconciler configures the Jobs that carry out changes.
//
// The service never writes to GitHub itself: it renders a document and
// spawns a Job holding the write credential. Everything here describes that
// Job.
type Reconciler struct {
	// Image runs the reconciler: this service's own image, whose `apply`
	// subcommand is the Job entrypoint. Pinned by digest or tag in the
	// deployment, never floating.
	Image string `yaml:"image"`
	// Namespace is where Jobs are created. Empty means the pod's own,
	// which is what the chart's RBAC is scoped to.
	Namespace string `yaml:"namespace"`
	// ServiceAccount the Jobs run as. Empty means the namespace default —
	// the Job needs no Kubernetes permissions of its own, only GitHub
	// ones, which arrive as a mounted Secret.
	ServiceAccount string `yaml:"serviceAccount"`
	// MinAdmins makes the reconciler refuse a configuration naming fewer
	// owners. A guard that always trips is one nobody reads, so it is
	// configurable rather than assumed.
	MinAdmins int `yaml:"minAdmins"`
	// Timeout bounds one run.
	Timeout time.Duration `yaml:"timeout"`
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
	// ProbeGroup is the source's health canary — see Directory.ProbeGroup.
	ProbeGroup string `yaml:"probeGroup,omitempty"`
}

// Company is one company: a directory, and optionally the GitHub
// organization it owns.
type Company struct {
	// Directory is the company's corporate directory.
	Directory Directory `yaml:"directory"`

	// GitHub is the company's organization, absent for companies that are
	// referenced only as partners (their people appear in another org's
	// partner-* teams; their own org comes later).
	GitHub *CompanyGitHub `yaml:"github"`
}

// Directory is a company's corporate directory: who exists, who is
// suspended, and who is in which group.
type Directory struct {
	// SSMPrefix holds the directory credentials — a service-account key
	// and the admin subject to impersonate.
	SSMPrefix string `yaml:"ssmPrefix"`
	// Domains restricts the source to these email domains. A directory
	// may serve several domains while this instance is only responsible
	// for some of them.
	Domains []string `yaml:"domains"`
	// ProbeGroup is the health canary: a group that always exists (the
	// directory's all@, typically). When set, the source is healthy as
	// long as users and the probe read — and a mapped group answering
	// 404 becomes a per-group absence whose teams fail safe
	// individually, instead of poisoning the whole source. Optional.
	ProbeGroup string `yaml:"probeGroup,omitempty"`
}

// CompanyGitHub is the organization a company owns, plus the credentials
// split that keeps the web tier read-only.
type CompanyGitHub struct {
	// Org is the organization login on github.com.
	Org string `yaml:"org"`

	ConsoleAppSSM string `yaml:"consoleAppSSM"`
	ApplierAppSSM string `yaml:"applierAppSSM"`
	ApplierSecret string `yaml:"applierSecret"`

	Exceptions []string        `yaml:"exceptions"`
	Teams      map[string]Team `yaml:"teams"`

	// MinAdmins overrides reconciler.minAdmins for this organization.
	// Organizations differ: a guard sized for one refuses every plan for
	// a smaller one. 0 means the global value.
	MinAdmins int `yaml:"minAdmins"`

	// ReconcileEnabled turns the continuous loop on for this organization.
	// Born false — the day-0 gate: nothing runs unattended until an
	// operator flips it after a supervised first sync (the postmortem
	// rule, as configuration). While false the loop still computes and
	// shows what it would do.
	ReconcileEnabled bool `yaml:"reconcileEnabled"`
}

// Org is one GitHub organization under management.
type Org struct {
	Name string `yaml:"name"`

	// Company is the code of the company owning this organization — and
	// therefore the name of the directory source that governs standing
	// in it (the home-company rule): a person's identity in the org's
	// own company decides their liveness here when one exists.
	Company string `yaml:"-"`

	// ConsoleAppSSM holds the read-only App credentials used by the web
	// tier. ApplierAppSSM holds the write-capable App credentials, which
	// are only ever mounted into a reconciler Job — never read by the web
	// process. Keeping them apart here mirrors the runtime boundary.
	ConsoleAppSSM string `yaml:"consoleAppSSM"`
	ApplierAppSSM string `yaml:"applierAppSSM"`

	// ApplierSecret names the Kubernetes Secret holding the applier App's
	// private key. It is mounted into reconciler Jobs and never read by
	// this process. Empty defaults to "roster-applier-<org>".
	ApplierSecret string `yaml:"applierSecret"`

	// Exceptions are logins the service never touches in either direction:
	// Apps, bots, and anything else whose membership is not a person's.
	Exceptions []string `yaml:"exceptions"`

	// Teams whose membership this service reconciles. Creating and deleting
	// teams is not this service's business — see docs/architecture.
	Teams map[string]Team `yaml:"teams"`

	// MinAdmins is this organization's owner-guard override; 0 means the
	// global reconciler value.
	MinAdmins int `yaml:"minAdmins"`

	// ReconcileEnabled turns the continuous loop on for this organization
	// (the day-0 gate; born false). See CompanyGitHub.ReconcileEnabled.
	ReconcileEnabled bool `yaml:"reconcileEnabled"`
}

// Team is directory-mapped (groups and/or explicit members) or pinned,
// never both.
type Team struct {
	// Groups are directory groups whose union is the team's membership.
	// Read flat: a nested group is not expanded, because a team whose
	// membership you cannot determine by looking is not reviewable.
	Groups []string `yaml:"groups"`

	// Members are explicit member emails, unioned with Groups. A group is
	// an automation for maintaining a member list; the target state is
	// the same either way, and a team can be declared by list before its
	// group exists. An explicit member must still be LIVE in the
	// directory owning their email domain — a static list never
	// resurrects a suspended person.
	Members []string `yaml:"members"`

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
	// Prefix roots every record inside the bucket. Empty for a dedicated
	// bucket (production); set for the shared-tier test model, where many
	// installs share one bucket and each claims "<namespace>/<release>/"
	// — the tenancy convention in gitops docs/architecture/
	// test-installations.md. Must end with "/" when set.
	Prefix string `yaml:"prefix"`
	// PrefixPerOrg files each record under its organization, which is what
	// lets one bucket policy per organization exist later.
	PrefixPerOrg bool `yaml:"prefixPerOrg"`
}

// Reconcile configures the continuous reconcile loop (the 0.17 model that
// supersedes the removals-only Schedule — see
// docs/architecture/reconciliation.md). Per-organization enablement is the
// day-0 gate and lives on each company's GitHub org (born disabled).
type Reconcile struct {
	// Interval is how often each enabled organization is reconciled. The
	// loop also runs on demand (an operator edit, or Sync now). 0 falls
	// back to the default.
	Interval time.Duration `yaml:"interval"`
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
		Reconciler: Reconciler{
			MinAdmins: 1,
			Timeout:   10 * time.Minute,
		},
		Reconcile: Reconcile{
			Interval: 15 * time.Minute,
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

	cfg.derive()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// derive builds the runtime Sources and Orgs views from Companies —
// sorted by company code, so everything downstream is deterministic.
func (c *Config) derive() {
	codes := make([]string, 0, len(c.Companies))
	for code := range c.Companies {
		codes = append(codes, code)
	}

	sort.Strings(codes)

	c.Sources = c.Sources[:0]
	c.Orgs = c.Orgs[:0]

	for _, code := range codes {
		company := c.Companies[code]

		c.Sources = append(c.Sources, Source{
			Name:       code,
			SSMPrefix:  company.Directory.SSMPrefix,
			Domains:    company.Directory.Domains,
			ProbeGroup: company.Directory.ProbeGroup,
		})

		if gh := company.GitHub; gh != nil {
			c.Orgs = append(c.Orgs, Org{
				Name:             gh.Org,
				Company:          code,
				ConsoleAppSSM:    gh.ConsoleAppSSM,
				ApplierAppSSM:    gh.ApplierAppSSM,
				ApplierSecret:    gh.ApplierSecret,
				Exceptions:       gh.Exceptions,
				Teams:            gh.Teams,
				MinAdmins:        gh.MinAdmins,
				ReconcileEnabled: gh.ReconcileEnabled,
			})
		}
	}
}

// MinAdminsFor resolves the owner guard for one organization: its own
// override when set, the global reconciler value otherwise.
func (c *Config) MinAdminsFor(org string) int {
	if o, ok := c.Org(org); ok && o.MinAdmins > 0 {
		return o.MinAdmins
	}

	return c.Reconciler.MinAdmins
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

	if err := c.validateCompanies(); err != nil {
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

	if c.Audit.Prefix != "" && !strings.HasSuffix(c.Audit.Prefix, "/") {
		// A prefix without the slash silently glues onto the first key
		// segment and two tenants' records interleave — the exact
		// collision the prefix exists to prevent.
		return fmt.Errorf("audit.prefix %q must end with %q", c.Audit.Prefix, "/")
	}

	// Zero DISABLES the unattended loop — the day-0 gate: nothing
	// unattended runs until the operator-reviewed first sync has. It must
	// be stated explicitly (`0s`); negative is still a mistake.
	if c.Schedule.RemovalsInterval < 0 {
		return fmt.Errorf("schedule.removalsInterval must be zero (disabled) or positive, got %s", c.Schedule.RemovalsInterval)
	}

	if c.Schedule.MaxRemovalFraction < 0 || c.Schedule.MaxRemovalFraction > 1 {
		return fmt.Errorf("schedule.maxRemovalFraction must be within [0,1], got %v", c.Schedule.MaxRemovalFraction)
	}

	if c.Reconcile.Interval < 0 {
		return fmt.Errorf("reconcile.interval must be zero (default) or positive, got %s", c.Reconcile.Interval)
	}

	if c.Reconciler.Timeout <= 0 {
		return fmt.Errorf("reconciler.timeout must be positive, got %s", c.Reconciler.Timeout)
	}

	if c.Reconciler.MinAdmins < 1 {
		// Zero would let a rendered document remove every owner and
		// peribolos would not object.
		return fmt.Errorf("reconciler.minAdmins must be at least 1, got %d", c.Reconciler.MinAdmins)
	}

	return nil
}

// ApplierSecretName is the Secret holding this organization's write
// credential.
func (o *Org) ApplierSecretName() string {
	if o.ApplierSecret != "" {
		return o.ApplierSecret
	}

	return "roster-applier-" + o.Name
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

// codePattern constrains company codes: they become source names and
// team-name prefixes (partner-<code>-...), so they share the DNS-ish
// discipline.
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// teamPattern and partnerPattern are the identity-model naming
// invariant: a directory-mapped team is either the owning company's
// (team-<x> ← team-<x>@<own domain>) or a partner company's
// (partner-<code>-team-<x> ← team-<x>@<code's domain>). The name alone
// tells you whose offboarding governs the team.
var (
	teamPattern    = regexp.MustCompile(`^team-[a-z0-9-]+$`)
	partnerPattern = regexp.MustCompile(`^partner-([a-z][a-z0-9]*)-(team-[a-z0-9-]+)$`)
)

func (c *Config) validateCompanies() error {
	if len(c.Companies) == 0 {
		return fmt.Errorf("companies is required: at least one company must be configured")
	}

	for code, company := range c.Companies {
		if !codePattern.MatchString(code) {
			return fmt.Errorf("companies[%q]: company code must be lowercase alphanumeric starting with a letter", code)
		}

		if company.Directory.SSMPrefix == "" {
			return fmt.Errorf("companies[%q].directory.ssmPrefix is required", code)
		}

		if len(company.Directory.Domains) == 0 {
			return fmt.Errorf("companies[%q].directory.domains is required: name the domains this company's directory is responsible for", code)
		}

		if probe := company.Directory.ProbeGroup; probe != "" {
			domain, ok := emailDomain(probe)
			if !ok {
				return fmt.Errorf("companies[%q].directory.probeGroup %q is not a group address", code, probe)
			}

			if !containsFold(company.Directory.Domains, domain) {
				return fmt.Errorf("companies[%q].directory.probeGroup %q is outside the directory's domains %v", code, probe, company.Directory.Domains)
			}
		}

		if gh := company.GitHub; gh != nil {
			if gh.Org == "" {
				return fmt.Errorf("companies[%q].github.org is required", code)
			}

			if err := c.validateTeamInvariant(code, gh); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateTeamInvariant enforces the naming contract per team.
func (c *Config) validateTeamInvariant(code string, gh *CompanyGitHub) error {
	own := c.Companies[code]

	for name, team := range gh.Teams {
		// Structural rules first, so a malformed team gets the message
		// about its actual problem rather than a family mismatch.
		switch {
		case !dns1123.MatchString(name):
			return fmt.Errorf("companies[%q].github.teams[%q]: team name must be lowercase alphanumeric with dashes", code, name)
		case team.Pinned && (len(team.Groups) > 0 || len(team.Members) > 0):
			return fmt.Errorf("companies[%q].github.teams[%q]: a team is either pinned or directory-mapped, not both", code, name)
		case !team.Pinned && len(team.Groups) == 0 && len(team.Members) == 0:
			return fmt.Errorf("companies[%q].github.teams[%q]: needs groups and/or members, or pinned: true", code, name)
		}

		for _, email := range team.Members {
			if email != strings.ToLower(email) || !strings.Contains(email, "@") {
				return fmt.Errorf("companies[%q].github.teams[%q]: member %q must be a lowercase email", code, name, email)
			}
		}

		if team.Pinned {
			// Pinned teams are for non-humans (robots) and interim pins;
			// they must not masquerade as directory-mapped families.
			if teamPattern.MatchString(name) || partnerPattern.MatchString(name) {
				return fmt.Errorf("companies[%q].github.teams[%q]: the team-*/partner-* families are directory-mapped by contract; pinned teams need a name outside them", code, name)
			}

			continue
		}

		switch m := partnerPattern.FindStringSubmatch(name); {
		case m != nil:
			partner, ok := c.Companies[m[1]]
			if !ok {
				return fmt.Errorf("companies[%q].github.teams[%q]: partner company %q is not configured", code, name, m[1])
			}

			if err := groupsMatch(team.Groups, m[2], partner.Directory.Domains); err != nil {
				return fmt.Errorf("companies[%q].github.teams[%q]: %w", code, name, err)
			}

		case teamPattern.MatchString(name):
			if err := groupsMatch(team.Groups, name, own.Directory.Domains); err != nil {
				return fmt.Errorf("companies[%q].github.teams[%q]: %w", code, name, err)
			}

		default:
			return fmt.Errorf("companies[%q].github.teams[%q]: directory-mapped team names must be team-<x> or partner-<code>-team-<x> (identity-model.md)", code, name)
		}
	}

	return nil
}

// groupsMatch requires every group to be <local>@<domain> with the local
// part equal to the team name and the domain owned by the right company.
func groupsMatch(groups []string, local string, domains []string) error {
	allowed := make(map[string]bool, len(domains))
	for _, d := range domains {
		allowed[strings.ToLower(d)] = true
	}

	for _, group := range groups {
		gotLocal, domain, ok := strings.Cut(strings.ToLower(group), "@")
		if !ok {
			return fmt.Errorf("group %q is not an email address", group)
		}

		if gotLocal != local {
			return fmt.Errorf("group %q must be named %s@<company domain> — the group's local part and the team name are the same thing by contract", group, local)
		}

		if !allowed[domain] {
			return fmt.Errorf("group %q is outside the owning company's domains %v", group, domains)
		}
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
		case team.Pinned && (len(team.Groups) > 0 || len(team.Members) > 0):
			return fmt.Errorf("orgs[%q].teams[%q]: a team is either pinned or directory-mapped, not both", o.Name, name)
		case !team.Pinned && len(team.Groups) == 0 && len(team.Members) == 0:
			return fmt.Errorf("orgs[%q].teams[%q]: needs groups and/or members, or pinned: true", o.Name, name)
		}
	}

	return nil
}

// MappedGroupsForDomains returns the mapped groups whose domain belongs
// to the given set — what a SINGLE directory source is asked to resolve.
// A source must never be asked about another company's groups: its
// service account has no rights there, and one foreign 403 fails the
// whole fetch, marking the source unhealthy (observed on kernel, day
// one). Cross-company membership still works because every group is
// fetched from its OWN company's source and memberships merge in the
// join.
func (c *Config) MappedGroupsForDomains(domains []string) []string {
	allowed := make(map[string]bool, len(domains))
	for _, d := range domains {
		allowed[strings.ToLower(d)] = true
	}

	var out []string

	for _, group := range c.MappedGroups() {
		if _, domain, ok := strings.Cut(strings.ToLower(group), "@"); ok && allowed[domain] {
			out = append(out, group)
		}
	}

	return out
}

// MappedGroups returns every directory group any team draws membership
// from, deduplicated.
//
// This is what a directory source is asked to resolve. Fetching only the
// groups some team actually maps keeps the read proportional to what the
// service uses, rather than to how many groups the directory happens to
// hold.
func (c *Config) MappedGroups() []string {
	seen := map[string]bool{}
	groups := []string{}

	for i := range c.Orgs {
		for _, team := range c.Orgs[i].Teams {
			for _, group := range team.Groups {
				key := strings.ToLower(group)
				if seen[key] {
					continue
				}

				seen[key] = true

				groups = append(groups, group)
			}
		}
	}

	sort.Strings(groups)

	return groups
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

// emailDomain returns the lowercased domain of a group address.
func emailDomain(address string) (string, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(address), "@")
	if !ok || local == "" || domain == "" {
		return "", false
	}

	return domain, true
}

// containsFold reports whether list contains value, case-insensitively.
func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(item, value) {
			return true
		}
	}

	return false
}
