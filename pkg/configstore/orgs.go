// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package configstore

import (
	"context"
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
	fieldGroups        = "groups"     // per-team, comma-separated
	fieldMembers       = "members"    // per-team, comma-separated
	segTeams           = "teams"
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
	// PutApp stores an org's GitHub App credentials (from the manifest flow).
	PutApp(ctx context.Context, org string, creds AppCredentials) error
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

		out = append(out, config.Org{
			Name:          name,
			MinAdmins:     atoiOrZero(c.scalars[fieldMinAdmins]),
			ConsoleAppSSM: console,
			ApplierAppSSM: c.scalars[fieldApplierAppSSM],
			Exceptions:    splitList(c.scalars[fieldExceptions]),
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

// appPath is the SSM parameter path for one App-credential field of an org.
func (s *OrgSSM) appPath(org, field string) string {
	return s.prefix + org + "/app/" + field
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

	return nil
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
