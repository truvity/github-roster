// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package configstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/truvity/github-roster/pkg/config"
)

// GitHub App credential fields written under <prefix>orgs/<name>/app/. These
// names MUST match what the reader (pkg/app buildOrgCache) reads, so a
// UI-created App and a Helm/ESO-provisioned one share one layout.
const (
	fieldAppID          = "github-app-id"
	fieldInstallationID = "github-installation-id"
	fieldPrivateKey     = "github-private-key"
	fieldClientID       = "github-client-id"
	fieldClientSecret   = "github-client-secret"  //nolint:gosec // SSM parameter name, not a secret value
	fieldWebhookSecret  = "github-webhook-secret" //nolint:gosec // SSM parameter name, not a secret value
)

// AppCredentials is what the manifest flow captures for an org's App. The
// installation id is 0 until the Org Owner installs the App; the private key
// and the secrets are stored encrypted (SecureString).
type AppCredentials struct {
	AppID          int64
	InstallationID int64
	PrivateKey     string
	ClientID       string
	ClientSecret   string
	WebhookSecret  string
}

// Organization fields, one SSM parameter each. Scalars live at
// <prefix>orgs/<name>/<field>; per-team lists at
// <prefix>orgs/<name>/teams/<team>/<field>.
const (
	fieldMinAdmins     = "minAdmins"
	fieldConsoleAppSSM = "consoleAppSSM"
	fieldApplierAppSSM = "applierAppSSM"
	fieldExceptions    = "exceptions" // comma-separated
	fieldProvenance    = "provenance" // "manual" (adopted) or "roster" (UI-created)
	fieldGroups        = "groups"     // per-team, comma-separated
	fieldMembers       = "members"    // per-team, comma-separated
	segTeams           = "teams"
)

// Provenance values recorded for a store org's App.
const (
	ProvenanceManual = "manual" // a pre-existing App, adopted into the store
	ProvenanceRoster = "roster" // created via the App-manifest flow
)

// OrgReader lists operator-added organizations with their teams. The team
// group/member lists carry no secret; the App credentials PutApp writes are
// SecureString and never surfaced by the reader.
type OrgReader interface {
	ListOrgs(ctx context.Context) ([]config.Org, error)
}

// OrgStore adds the write half used by the App-manifest flow.
type OrgStore interface {
	OrgReader
	// PutOrg stages an organization (scalars + teams + credential pointer),
	// born reconcile-disabled and credential-less, ready for the App flow.
	PutOrg(ctx context.Context, org config.Org) error
	// PutApp stores an org's GitHub App credentials (from the manifest flow).
	PutApp(ctx context.Context, org string, creds AppCredentials) error
	// PutProvenance records how the org's App came to be (manual/roster).
	PutProvenance(ctx context.Context, org, provenance string) error
	// PutTeam creates or replaces one team's mapping on a STORE org — the
	// operator's team↔group editor. Git-declared orgs are never touched
	// through this store (the git layer is the reviewed baseline).
	PutTeam(ctx context.Context, org, team string, cfg config.Team) error
	// DeleteTeam removes one team's mapping from a store org.
	DeleteTeam(ctx context.Context, org, team string) error
}

// OrgSSM reads organizations under <prefix>orgs/.
type OrgSSM struct {
	client *ssm.Client
	prefix string
}

// NewOrgSSM roots an org store at prefix (e.g. "/roster/"), reading its
// "orgs/" segment.
func NewOrgSSM(client *ssm.Client, prefix string) *OrgSSM {
	return &OrgSSM{client: client, prefix: prefix + "orgs/"}
}

// collectedOrg accumulates one organization's parameters before it is turned
// into a config.Org.
type collectedOrg struct {
	scalars map[string]string
	teams   map[string]map[string]string
}

// ListOrgs reads every stored organization in one paginated sweep. Malformed
// entries are skipped, not fatal — and, load-bearing for safety, an org that
// is not fully formed (no credential pointer, or no non-empty team) is dropped
// entirely: surfacing a team-less org would present the reconciler with "this
// org should have no members" and drive removals. Store orgs are also always
// born reconcile-disabled; enabling is the separate control path, never here.
func (s *OrgSSM) ListOrgs(ctx context.Context) ([]config.Org, error) {
	byName := map[string]*collectedOrg{}

	paginator := ssm.NewGetParametersByPathPaginator(s.client, &ssm.GetParametersByPathInput{
		Path:      aws.String(s.prefix),
		Recursive: aws.Bool(true),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list organizations under %q: %w", s.prefix, err)
		}

		for i := range page.Parameters {
			name, team, field, ok := s.splitOrgParam(aws.ToString(page.Parameters[i].Name))
			if !ok {
				continue
			}

			c := byName[name]
			if c == nil {
				c = &collectedOrg{scalars: map[string]string{}, teams: map[string]map[string]string{}}
				byName[name] = c
			}

			value := aws.ToString(page.Parameters[i].Value)

			if team == "" {
				c.scalars[field] = value
				continue
			}

			if c.teams[team] == nil {
				c.teams[team] = map[string]string{}
			}

			c.teams[team][field] = value
		}
	}

	return orgsFrom(byName), nil
}

// splitOrgParam parses "<prefix>orgs/<name>/<field>" (scalar) or
// "<prefix>orgs/<name>/teams/<team>/<field>" (per-team). team is "" for a
// scalar field.
func (s *OrgSSM) splitOrgParam(full string) (name, team, field string, ok bool) {
	rest := strings.TrimPrefix(full, s.prefix)
	if rest == full {
		return "", "", "", false
	}

	parts := strings.Split(rest, "/")

	switch {
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return parts[0], "", parts[1], true
	case len(parts) == 4 && parts[0] != "" && parts[1] == segTeams && parts[2] != "" && parts[3] != "":
		return parts[0], parts[2], parts[3], true
	default:
		return "", "", "", false
	}
}

// orgsFrom turns the per-org collected parameters into config.Org values,
// dropping any that are not fully formed (see ListOrgs).
func orgsFrom(byName map[string]*collectedOrg) []config.Org {
	out := make([]config.Org, 0, len(byName))

	for name, c := range byName {
		console := c.scalars[fieldConsoleAppSSM]
		if console == "" {
			continue // no credential pointer — not a usable org
		}

		teams := make(map[string]config.Team, len(c.teams))
		for tname, tf := range c.teams {
			groups := splitList(tf[fieldGroups])
			members := splitList(tf[fieldMembers])

			if len(groups) == 0 && len(members) == 0 {
				continue // an empty team carries no intent
			}

			teams[tname] = config.Team{Groups: groups, Members: members}
		}

		if len(teams) == 0 {
			continue // a team-less org must never surface (it would drive removals)
		}

		provenance := c.scalars[fieldProvenance]
		if provenance == "" {
			provenance = ProvenanceManual // a store org with no tag is an adopted one
		}

		out = append(out, config.Org{
			Name:          name,
			MinAdmins:     atoiOrZero(c.scalars[fieldMinAdmins]),
			ConsoleAppSSM: console,
			ApplierAppSSM: c.scalars[fieldApplierAppSSM],
			Exceptions:    splitList(c.scalars[fieldExceptions]),
			Provenance:    provenance,
			Teams:         teams,
			// ReconcileEnabled is deliberately left false: a store org is born
			// disabled, exactly like a git org's day-0 state.
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

func atoiOrZero(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))

	return n
}

// MergeOrgs overlays the git-declared organizations on top of the store: a git
// org wins by name (case-insensitive), store-only orgs are appended. Mirrors
// MergeDirectories.
func MergeOrgs(iac, store []config.Org) []config.Org {
	seen := make(map[string]bool, len(iac))
	for i := range iac {
		seen[strings.ToLower(iac[i].Name)] = true
	}

	out := append([]config.Org(nil), iac...)

	for i := range store {
		if seen[strings.ToLower(store[i].Name)] {
			continue // shadowed by git
		}

		out = append(out, store[i])
	}

	return out
}

// orgAppPath is the SSM path prefix under which an org's App credentials
// live. It is what consoleAppSSM points at (so the reader finds the creds)
// and what appPath appends field names to.
func (s *OrgSSM) orgAppPath(org string) string {
	return s.prefix + org + "/app"
}

// appPath is the SSM parameter path for one App-credential field of an org.
func (s *OrgSSM) appPath(org, field string) string {
	return s.orgAppPath(org) + "/" + field
}

// orgParam is one plain-String SSM parameter that stages an organization.
type orgParam struct {
	name  string
	value string
}

// orgParams is the flat list of plain-String parameters that stage an
// organization: the credential pointer (consoleAppSSM → the org's own app/
// path), the optional scalars, and each team's group/member lists. Pure (no
// I/O) and deterministically ordered so it is unit-tested; PutOrg writes it.
// Credentials are NOT here — PutApp fills those once the App is created.
func (s *OrgSSM) orgParams(org config.Org) []orgParam {
	name := strings.TrimSpace(org.Name)

	params := []orgParam{
		{s.prefix + name + "/" + fieldConsoleAppSSM, s.orgAppPath(name)},
	}

	if org.MinAdmins > 0 {
		params = append(params, orgParam{s.prefix + name + "/" + fieldMinAdmins, strconv.Itoa(org.MinAdmins)})
	}

	if len(org.Exceptions) > 0 {
		params = append(params, orgParam{s.prefix + name + "/" + fieldExceptions, strings.Join(org.Exceptions, ",")})
	}

	tnames := make([]string, 0, len(org.Teams))
	for tname := range org.Teams {
		tnames = append(tnames, tname)
	}

	sort.Strings(tnames)

	for _, tname := range tnames {
		t := org.Teams[tname]
		base := s.prefix + name + "/" + segTeams + "/" + strings.TrimSpace(tname) + "/"

		if len(t.Groups) > 0 {
			params = append(params, orgParam{base + fieldGroups, strings.Join(t.Groups, ",")})
		}

		if len(t.Members) > 0 {
			params = append(params, orgParam{base + fieldMembers, strings.Join(t.Members, ",")})
		}
	}

	return params
}

// PutOrg stages an organization in the store: its scalar fields, its teams,
// and the consoleAppSSM pointer at its own app/ path so the reader (and
// orgsFrom's safety filter) treat it as a usable org once the App flow fills
// the credentials. Requires a name and at least one non-empty team — a
// team-less org would drive member removals (see ListOrgs). Born reconcile-
// disabled; enabling is the separate control path.
func (s *OrgSSM) PutOrg(ctx context.Context, org config.Org) error {
	name := strings.TrimSpace(org.Name)
	if name == "" {
		return fmt.Errorf("org name is required")
	}

	hasTeam := false
	for _, t := range org.Teams {
		if len(t.Groups) > 0 || len(t.Members) > 0 {
			hasTeam = true

			break
		}
	}

	if !hasTeam {
		return fmt.Errorf("at least one team with a group or member is required")
	}

	for _, p := range s.orgParams(org) {
		if err := s.putParam(ctx, p.name, p.value, types.ParameterTypeString); err != nil {
			return err
		}
	}

	return nil
}

// PutApp stores an org's App credentials under its store prefix. The private
// key and the client/webhook secrets go in as SecureString; the ids as plain
// String. A zero installation id is skipped — it is filled in once the App is
// installed on the organization.
func (s *OrgSSM) PutApp(ctx context.Context, org string, creds AppCredentials) error {
	if org == "" || creds.AppID == 0 || creds.PrivateKey == "" {
		return fmt.Errorf("org, app id and private key are required")
	}

	plain := map[string]string{
		fieldAppID:    strconv.FormatInt(creds.AppID, 10),
		fieldClientID: creds.ClientID,
	}
	if creds.InstallationID != 0 {
		plain[fieldInstallationID] = strconv.FormatInt(creds.InstallationID, 10)
	}

	secret := map[string]string{
		fieldPrivateKey:    creds.PrivateKey,
		fieldClientSecret:  creds.ClientSecret,
		fieldWebhookSecret: creds.WebhookSecret,
	}

	for field, value := range plain {
		if err := s.putParam(ctx, s.appPath(org, field), value, types.ParameterTypeString); err != nil {
			return err
		}
	}

	for field, value := range secret {
		if value == "" {
			continue
		}

		if err := s.putParam(ctx, s.appPath(org, field), value, types.ParameterTypeSecureString); err != nil {
			return err
		}
	}

	// Ensure the reader can find these credentials even if the org was seeded
	// credentials-first (e.g. Helm/ESO wrote app/ but no scalar): point
	// consoleAppSSM at this app path. Idempotent with PutOrg's own write.
	return s.putParam(ctx, s.prefix+org+"/"+fieldConsoleAppSSM, s.orgAppPath(org), types.ParameterTypeString)
}

func (s *OrgSSM) putParam(ctx context.Context, name, value string, t types.ParameterType) error {
	if _, err := s.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(name),
		Value:     aws.String(value),
		Type:      t,
		Overwrite: aws.Bool(true),
	}); err != nil {
		return fmt.Errorf("write %q: %w", name, err)
	}

	return nil
}

// PutProvenance records how an org's App was created (manual|roster).
func (s *OrgSSM) PutProvenance(ctx context.Context, org, provenance string) error {
	if org == "" || provenance == "" {
		return fmt.Errorf("org and provenance are required")
	}

	return s.putParam(ctx, s.prefix+org+"/"+fieldProvenance, provenance, types.ParameterTypeString)
}

// PutTeam creates or replaces one team's mapping on a store org. A field that
// is now empty is deleted, not left behind — a stale groups parameter would
// keep granting a membership the operator just took away.
func (s *OrgSSM) PutTeam(ctx context.Context, org, team string, cfg config.Team) error {
	org, team = strings.TrimSpace(org), strings.TrimSpace(team)
	if org == "" || team == "" {
		return fmt.Errorf("org and team are required")
	}

	if len(cfg.Groups) == 0 && len(cfg.Members) == 0 {
		return fmt.Errorf("team %q: at least one group or member is required (delete the team to unmap it)", team)
	}

	base := s.prefix + org + "/" + segTeams + "/" + team + "/"

	fields := map[string]string{}
	if len(cfg.Groups) > 0 {
		fields[fieldGroups] = strings.Join(cfg.Groups, ",")
	}

	if len(cfg.Members) > 0 {
		fields[fieldMembers] = strings.Join(cfg.Members, ",")
	}

	for field, value := range fields {
		if err := s.putParam(ctx, base+field, value, types.ParameterTypeString); err != nil {
			return err
		}
	}

	for _, field := range []string{fieldGroups, fieldMembers} {
		if _, keep := fields[field]; keep {
			continue
		}

		if err := s.deleteParam(ctx, base+field); err != nil {
			return err
		}
	}

	return nil
}

// DeleteTeam removes one team's mapping from a store org.
func (s *OrgSSM) DeleteTeam(ctx context.Context, org, team string) error {
	org, team = strings.TrimSpace(org), strings.TrimSpace(team)
	if org == "" || team == "" {
		return fmt.Errorf("org and team are required")
	}

	base := s.prefix + org + "/" + segTeams + "/" + team + "/"

	for _, field := range []string{fieldGroups, fieldMembers} {
		if err := s.deleteParam(ctx, base+field); err != nil {
			return err
		}
	}

	return nil
}

// deleteParam removes one parameter, tolerating absence.
func (s *OrgSSM) deleteParam(ctx context.Context, name string) error {
	_, err := s.client.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)})

	var notFound *types.ParameterNotFound
	if err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("delete %q: %w", name, err)
	}

	return nil
}
