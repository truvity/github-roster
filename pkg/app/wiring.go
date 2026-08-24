package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"

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
	// OrgStore lists operator-added organizations (Settings display) and
	// stores App credentials from the manifest flow.
	OrgStore configstore.OrgStore
	// gitSources are the git-declared directory Sources (their live clients
	// and caches survive reloads by name); cfg is retained so reloads can
	// re-derive each store directory's mapped groups. Both back
	// reloadDirectories, the console's live counterpart to the broker's.
	gitSources []directory.Source
	cfg        *config.Config
	// secretsReader + reloadableOrgs back reloadOrgs: each org's GitHub App
	// client is rebuilt live when its SSM credentials rotate, without a
	// restart. secretsReader re-reads the credentials each pass.
	secretsReader  *secrets.Reader
	reloadableOrgs []*reloadableOrg
}

// reloadableOrg wraps an org's cached GitHub reader behind an atomic pointer
// so the App client can be rebuilt on the fly. The org SET is git-static, so
// the Orgs map itself never changes (no map-swap race); only the reader inside
// each entry is swapped, and only when the credentials actually change (a
// fingerprint guard), which preserves the cache across no-op reloads.
type reloadableOrg struct {
	name    string
	org     *config.Org
	current atomic.Pointer[orgstate.Cache]
	// fp is the credentials fingerprint of the current reader. Read and
	// written only by the single reload goroutine (and once at startup), so
	// it needs no synchronization of its own; the reader swap is what
	// concurrent request handlers observe, and that goes through current.
	fp string
}

// Read serves the current reader; satisfies server.OrgReader.
func (r *reloadableOrg) Read(ctx context.Context) (*orgstate.State, error) {
	return r.current.Load().Read(ctx)
}

// Invalidate busts the current reader's cache; satisfies the server's
// invalidator so the applier's post-run cache drop still reaches through.
func (r *reloadableOrg) Invalidate() {
	r.current.Load().Invalidate()
}

// swapIfChanged installs a freshly-built reader only when the credentials
// fingerprint differs, so an unchanged reload keeps the warm cache. Returns
// whether it swapped. Called only from the reload goroutine.
func (r *reloadableOrg) swapIfChanged(cache *orgstate.Cache, fp string) bool {
	if fp == r.fp {
		return false
	}

	r.fp = fp
	r.current.Store(cache)

	return true
}

// reloadOrgs re-reads each org's GitHub App credentials and rebuilds its
// reader when they have rotated — the console's App-client live reload. A
// per-org read failure keeps the current client (best-effort, like the
// directory reload).
func (l *readLayers) reloadOrgs(ctx context.Context, logger *slog.Logger) {
	if l.secretsReader == nil {
		return
	}

	for _, ro := range l.reloadableOrgs {
		cache, fp, err := buildOrgCache(ctx, l.secretsReader, ro.org)
		if err != nil {
			logger.WarnContext(ctx, "org reload: rebuilding the App client failed; keeping current",
				"org", ro.name, "error", err)

			continue
		}

		if ro.swapIfChanged(cache, fp) {
			logger.InfoContext(ctx, "org reload: App client rebuilt (credentials rotated)", "org", ro.name)
		}
	}
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
		OrgStore: configstore.NewOrgSSM(ssmClient, cfg.Mapping.SSMPrefix),
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

	// secretsReader is retained so reloadOrgs can re-read rotated App
	// credentials without a restart.
	layers.secretsReader = reader

	for i := range cfg.Orgs {
		org := &cfg.Orgs[i]

		// The CONSOLE credential, deliberately. The applier's credentials
		// are never read by this process — they are mounted into the
		// reconciler Job and nowhere else.
		cache, fp, err := buildOrgCache(ctx, reader, org)
		if err != nil {
			return nil, err
		}

		// Behind a reloadable wrapper so a credential rotation swaps the
		// client live (the Orgs map stays stable — the org set is git-static).
		ro := &reloadableOrg{name: org.Name, org: org, fp: fp}
		ro.current.Store(cache)

		layers.Orgs[org.Name] = ro
		layers.reloadableOrgs = append(layers.reloadableOrgs, ro)
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

// buildOrgCache reads an org's GitHub App credentials and builds its cached
// reader, returning the reader and a fingerprint of the credentials so a live
// reload can tell a rotation from a no-op (and keep the warm cache on a no-op).
func buildOrgCache(ctx context.Context, reader *secrets.Reader, org *config.Org) (*orgstate.Cache, string, error) {
	values, err := reader.ReadAll(ctx, org.ConsoleAppSSM, fieldAppID, fieldInstallationID, fieldPrivateKey)
	if err != nil {
		return nil, "", fmt.Errorf("org %q console credentials: %w", org.Name, err)
	}

	fp := credFingerprint(values[fieldAppID], values[fieldInstallationID], values[fieldPrivateKey])

	appID, err := strconv.ParseInt(values[fieldAppID], 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("org %q: app id %q is not a number: %w", org.Name, values[fieldAppID], err)
	}

	installationID, err := strconv.ParseInt(values[fieldInstallationID], 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("org %q: installation id is not a number: %w", org.Name, err)
	}

	source, err := githubapp.NewTokenSource(githubapp.Credentials{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKey:     []byte(values[fieldPrivateKey]),
	})
	if err != nil {
		return nil, "", fmt.Errorf("org %q: %w", org.Name, err)
	}

	orgReader, err := orgstate.NewReader(source, org.Name, "")
	if err != nil {
		return nil, "", err
	}

	// Cached, with the sync/removals paths bypassing via ReadFresh and every
	// applier run invalidating (pkg/orgstate/cache.go).
	return orgstate.NewCache(org.Name, orgReader), fp, nil
}

// credFingerprint is a stable hash of an org's App credentials, used to detect
// a rotation on reload without holding the private key around for comparison.
func credFingerprint(appID, installationID, privateKey string) string {
	// The unit separator keeps the three fields unambiguous.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x1f%s\x1f%s", appID, installationID, privateKey)))

	return hex.EncodeToString(sum[:])
}
