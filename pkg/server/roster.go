package server

import (
	"context"
	"fmt"
	"time"

	"github.com/truvity/github-roster/pkg/orgstate"
	"github.com/truvity/github-roster/pkg/roster"
)

// buildRoster gathers the three read layers and joins them.
//
// GitHub is read live on each call rather than cached. The console's whole
// job is telling an operator what is true right now, and a cached answer
// that is thirty seconds stale is exactly the sort of thing that makes
// someone confirm a sync they would otherwise have questioned. Directory
// data is the opposite — expensive, rate-limited, and refreshed on its own
// schedule — so that one comes from the cache.
func (d *Deps) buildRoster(ctx context.Context) (*roster.Roster, error) {
	if d.Mapping == nil {
		return nil, fmt.Errorf("no mapping store configured")
	}

	entries, err := d.Mapping.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("read mapping: %w", err)
	}

	in := roster.Inputs{
		Config:  d.Config,
		Entries: entries,
		Orgs:    map[string]*orgstate.State{},
		Now:     time.Now(),
	}

	if d.Directories != nil {
		in.SourceStatuses = d.Directories.Statuses()

		for _, cache := range d.Directories.Caches() {
			if snapshot, ok := cache.Snapshot(); ok {
				in.Snapshots = append(in.Snapshots, snapshot)
			}
		}
	}

	for name, reader := range d.Orgs {
		state, err := reader.Read(ctx)
		if err != nil {
			// One unreadable organization must not blank the whole roster:
			// the others are still true, and the join reports an absent
			// organization as "nothing known" rather than "nobody is a
			// member".
			d.Logger.ErrorContext(ctx, "reading organization failed; it will be absent from this roster",
				"org", name, "error", err)

			continue
		}

		in.Orgs[name] = state
	}

	return roster.Join(in), nil
}

// githubReadAt reports when each organization's GitHub state was read, so
// a page rendered from the cache can say how old its answer is instead of
// passing it off as live.
func (d *Deps) githubReadAt(ctx context.Context) map[string]time.Time {
	out := make(map[string]time.Time, len(d.Orgs))

	for name, reader := range d.Orgs {
		state, err := reader.Read(ctx)
		if err != nil {
			continue
		}

		out[name] = state.ReadAt
	}

	return out
}

// rosterResponse is the JSON body of GET /api/roster.
//
// huma derives the OpenAPI description from this type, which is what makes
// the schema a checkable contract for the puller in the gitops repository
// rather than a prose promise.
type rosterResponse struct {
	Body *roster.Roster
}
