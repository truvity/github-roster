package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/truvity/github-roster/pkg/audit"
	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/directory"
	"github.com/truvity/github-roster/pkg/githubapp"
	"github.com/truvity/github-roster/pkg/mapping"
	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/secrets"
	"github.com/truvity/github-roster/pkg/server"
)

// Field names in the mirrored credential sets. They match the mirror
// manifest, so a rename there is a compile-time-visible rename here.
const (
	fieldServiceAccountKey = "google-service-account-key-json"
	fieldAdminEmail        = "google-admin-email"

	fieldAppID          = "github-app-id"
	fieldInstallationID = "github-installation-id"
	fieldPrivateKey     = "github-private-key"
)

// readLayers is everything the service reads from.
type readLayers struct {
	Mapping     mapping.Store
	Directories *directory.Set
	// Orgs holds one reader per managed organization, keyed by org name.
	Orgs map[string]server.OrgReader
	// Audit records every run.
	Audit audit.Sink
}

// buildReadLayers constructs the read side from configuration.
//
// Credentials are read here, at startup, rather than lazily on first use: a
// missing parameter or a malformed key should stop the rollout, not surface
// as a broken page an hour later when somebody opens it.
func buildReadLayers(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*readLayers, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws configuration: %w", err)
	}

	ssmClient := ssm.NewFromConfig(awsCfg)
	reader := secrets.NewReader(ssmClient)

	var store mapping.Store = mapping.NewSSM(ssmClient, cfg.Mapping.SSMPrefix)

	// IaC people: merge git-declared entries read-only over the store.
	if len(cfg.People) > 0 {
		iac := make([]mapping.Entry, 0, len(cfg.People))
		for _, p := range cfg.People {
			iac = append(iac, mapping.Entry{
				Name:   p.Name,
				GitHub: p.GitHub,
				Emails: p.Emails,
				K8s:    p.K8s,
				Class:  mapping.Class(p.Class),
				Pinned: p.Pinned,
			})
		}

		overlay, err := mapping.NewOverlay(store, iac)
		if err != nil {
			return nil, err
		}

		store = overlay
	}

	layers := &readLayers{
		Mapping: store,
		Orgs:    make(map[string]server.OrgReader, len(cfg.Orgs)),
	}

	sources, err := buildSources(ctx, reader, cfg)
	if err != nil {
		return nil, err
	}

	layers.Directories = directory.NewSet(logger, sources...)

	// The audit sink. Required rather than optional: a deployment that
	// cannot record what it did should not be quietly acting anyway.
	layers.Audit, err = audit.NewS3(s3.NewFromConfig(awsCfg), cfg.Audit.Bucket, cfg.Audit.Prefix, cfg.Audit.PrefixPerOrg)
	if err != nil {
		return nil, err
	}

	for i := range cfg.Orgs {
		org := &cfg.Orgs[i]

		// The CONSOLE credential, deliberately. The applier's credentials
		// are never read by this process — they are mounted into the
		// reconciler Job and nowhere else.
		orgReader, err := buildOrgReader(ctx, reader, org)
		if err != nil {
			return nil, err
		}

		// Cached, with the sync/removals paths bypassing via ReadFresh
		// and every applier run invalidating (pkg/orgstate/cache.go).
		layers.Orgs[org.Name] = orgstate.NewCache(org.Name, orgReader)

	}

	return layers, nil
}

func buildSources(ctx context.Context, reader *secrets.Reader, cfg *config.Config) ([]directory.Source, error) {
	sources := make([]directory.Source, 0, len(cfg.Sources))

	for i := range cfg.Sources {
		src := &cfg.Sources[i]

		// A source with an endpoint reads through a DirectoryService
		// (google-group-sync) and needs no directory credential of its own.
		if src.Endpoint != "" {
			source, err := directory.NewResolver(directory.ResolverConfig{
				Name:       src.Name,
				Endpoint:   src.Endpoint,
				Domains:    src.Domains,
				Groups:     cfg.MappedGroupsForDomains(src.Domains),
				ProbeGroup: src.ProbeGroup,
			})
			if err != nil {
				return nil, err
			}

			sources = append(sources, source)

			continue
		}

		values, err := reader.ReadAll(ctx, src.SSMPrefix, fieldServiceAccountKey, fieldAdminEmail)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", src.Name, err)
		}

		source, err := directory.NewGoogle(directory.GoogleConfig{
			Name:    src.Name,
			Domains: src.Domains,
			// Only the groups some team maps to AND this source's
			// directory owns — a source asked about a foreign company's
			// group 403s and the whole fetch fails.
			Groups:     cfg.MappedGroupsForDomains(src.Domains),
			ProbeGroup: src.ProbeGroup,
			KeyJSON:    []byte(values[fieldServiceAccountKey]),
			Subject:    values[fieldAdminEmail],
		})
		if err != nil {
			return nil, err
		}

		sources = append(sources, source)
	}

	return sources, nil
}

func buildOrgReader(ctx context.Context, reader *secrets.Reader, org *config.Org) (*orgstate.Reader, error) {
	values, err := reader.ReadAll(ctx, org.ConsoleAppSSM, fieldAppID, fieldInstallationID, fieldPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("org %q console credentials: %w", org.Name, err)
	}

	appID, err := strconv.ParseInt(values[fieldAppID], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("org %q: app id %q is not a number: %w", org.Name, values[fieldAppID], err)
	}

	installationID, err := strconv.ParseInt(values[fieldInstallationID], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("org %q: installation id is not a number: %w", org.Name, err)
	}

	source, err := githubapp.NewTokenSource(githubapp.Credentials{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKey:     []byte(values[fieldPrivateKey]),
	})
	if err != nil {
		return nil, fmt.Errorf("org %q: %w", org.Name, err)
	}

	return orgstate.NewReader(source, org.Name, "")
}
