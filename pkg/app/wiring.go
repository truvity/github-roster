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
	"github.com/truvity/github-roster/pkg/configstore"
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
	// DirStore holds operator-added directories.
	DirStore configstore.DirectoryStore
	// gitSources are the git-declared directory Sources (their live clients
	// and caches survive reloads by name); cfg is retained so reloads can
	// re-derive each store directory's mapped groups. Both back
	// reloadDirectories, the console's live counterpart to the broker's.
	gitSources []directory.Source
	cfg        *config.Config
}

// reloadDirectories re-reads the directory store and reconciles the shared
// directory Set to git ∪ store (keeping git clients + caches, adding/removing
// store-backed resolvers). It is the CONSOLE's live-reload path: without it
// an operator-added directory (written to the store via Settings) is picked up
// by the broker at once but invisible on the console until restart. Simpler
// than the broker's reload — the console runs no removals, so there is no
// effective-config / expected-sources swap here, only the Set reconcile.
// Best-effort: a store read failure keeps the current view.
func (l *readLayers) reloadDirectories(ctx context.Context, logger *slog.Logger) {
	if l.DirStore == nil || l.Directories == nil {
		return
	}

	stored, err := l.DirStore.List(ctx)
	if err != nil {
		logger.WarnContext(ctx, "config reload: listing store directories failed; keeping current",
			"error", err)

		return
	}

	gitNames := make(map[string]bool, len(l.gitSources))
	for _, gs := range l.gitSources {
		gitNames[gs.Name()] = true
	}

	sources := append([]directory.Source(nil), l.gitSources...)

	for i := range stored {
		src := stored[i]
		if gitNames[src.Name] {
			continue // git wins by name
		}

		resolver, rerr := directory.NewResolver(directory.ResolverConfig{
			Name:       src.Name,
			Endpoint:   src.Endpoint,
			Domains:    src.Domains,
			Groups:     l.cfg.MappedGroupsForDomains(src.Domains),
			ProbeGroup: src.ProbeGroup,
		})
		if rerr != nil {
			logger.WarnContext(ctx, "config reload: skipping malformed store directory",
				"directory", src.Name, "error", rerr)

			continue
		}

		sources = append(sources, resolver)
	}

	if l.Directories.Reconcile(sources) {
		logger.InfoContext(ctx, "config reload: console directory set updated",
			"directories", len(sources))
	}
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
		Mapping:  store,
		DirStore: configstore.NewSSM(ssmClient, cfg.Mapping.SSMPrefix),
		Orgs:     make(map[string]server.OrgReader, len(cfg.Orgs)),
		cfg:      cfg,
	}

	// Git-declared directory sources, with their credentials. Operator-added
	// store directories are NOT merged statically here (as they once were):
	// reloadDirectories folds them in below and the console's reload ticker
	// keeps them current, so a directory added via Settings takes effect
	// without a restart — matching the broker.
	gitSources, err := buildSources(ctx, reader, cfg)
	if err != nil {
		return nil, err
	}

	layers.gitSources = gitSources
	layers.Directories = directory.NewSet(logger, gitSources...)

	// Fold in the store directories once, now, so the console serves them
	// from the first request rather than only after the first tick.
	layers.reloadDirectories(ctx, logger)

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
