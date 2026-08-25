package broker

import (
	"context"

	"github.com/truvity/github-roster/pkg/config"
	"github.com/truvity/github-roster/pkg/configstore"
	"github.com/truvity/github-roster/pkg/directory"
)

// effectiveConfig returns the current effective configuration: the git
// config with operator-added store directories merged into Sources. Until
// the first reload it is the git config. The removals fail-safe reads
// Sources for the domain→source (expected-sources) mapping, so a store
// directory is a first-class source here, not a half-integrated one.
func (d *Deps) effectiveConfig() *config.Config {
	if eff := d.cfgEff.Load(); eff != nil {
		return eff
	}

	return d.Config
}

// reloadDirectories re-reads the directory store, reconciles the shared
// directory Set to git ∪ store (keeping git clients and their caches,
// adding/removing store-backed ones), and swaps in the effective config.
// Best-effort: a store read failure keeps the current effective view.
// Called at the start of each reconcile pass (under the apply lock).
func (d *Deps) reloadDirectories(ctx context.Context) {
	if d.DirStore == nil || d.Directories == nil {
		return
	}

	stored, err := d.DirStore.List(ctx)
	if err != nil {
		d.Logger.WarnContext(ctx, "config reload: listing store directories failed; keeping current",
			"error", err)

		return
	}

	// Effective config: git with store directories merged into Sources
	// (git wins by name). Shallow copy — Orgs/Companies/maps are shared and
	// never mutated here; only the Sources slice is replaced.
	eff := *d.Config
	eff.Sources = configstore.MergeDirectories(d.Config.Sources, stored)
	d.cfgEff.Store(&eff)

	// Effective source clients: keep the git sources (their live clients +
	// caches survive in the Set by name) and add a resolver client for each
	// store directory not shadowed by a git source.
	gitNames := make(map[string]bool, len(d.GitSources))
	for _, gs := range d.GitSources {
		gitNames[gs.Name()] = true
	}

	sources := append([]directory.Source(nil), d.GitSources...)

	for i := range stored {
		src := stored[i]
		if gitNames[src.Name] {
			continue // git wins
		}

		resolver, rerr := directory.NewResolver(directory.ResolverConfig{
			Name:     src.Name,
			Endpoint: src.Endpoint,
			Domains:  src.DomainNames(),
			Groups:   eff.MappedGroupsForDomains(src.DomainNames()),
			Probes:   src.ProbeGroups(),
		})
		if rerr != nil {
			d.Logger.WarnContext(ctx, "config reload: skipping malformed store directory",
				"directory", src.Name, "error", rerr)

			continue
		}

		sources = append(sources, resolver)
	}

	if d.Directories.Reconcile(sources) {
		d.Logger.InfoContext(ctx, "config reload: directory set updated",
			"directories", len(sources))
	}
}
