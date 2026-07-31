package orgstate

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Cache TTLs. Split by how the data actually changes: teams change
// essentially only through this service's own syncs and reviewed
// infrastructure commits, membership changes with joiners and leavers.
// Both are backstopped by Invalidate after every applier run, so the TTL
// only covers out-of-band edits made directly in the GitHub UI.
const (
	// DefaultMembershipTTL bounds staleness of members and invitations.
	DefaultMembershipTTL = 2 * time.Minute
	// DefaultTeamsTTL bounds staleness of teams and their rosters.
	DefaultTeamsTTL = 10 * time.Minute
)

// PartReader reads the two halves of an organization's state. *Reader
// satisfies it.
type PartReader interface {
	ReadMembership(ctx context.Context) (*Membership, error)
	ReadTeams(ctx context.Context) (*TeamState, error)
}

// Cache wraps a reader with per-half TTLs.
//
// It exists for two reasons at once: an App installation token gets 5,000
// requests per hour and a single page render costs ~20, and a cached page
// renders in microseconds instead of seconds. It is deliberately NOT used
// by the sync surfaces — the state an operator confirms, and the state a
// removals sweep decides on, must be the truth right now (ReadFresh).
// State.ReadAt survives the cache, so the UI can always say how old its
// answer is.
type Cache struct {
	org    string
	reader PartReader

	membershipTTL time.Duration
	teamsTTL      time.Duration

	// now is replaceable so tests can move time rather than sleep through
	// it.
	now func() time.Time

	mu         sync.Mutex
	membership *Membership
	teams      *TeamState
}

// NewCache wraps a reader with the default TTLs.
func NewCache(org string, reader PartReader) *Cache {
	return &Cache{
		org:           org,
		reader:        reader,
		membershipTTL: DefaultMembershipTTL,
		teamsTTL:      DefaultTeamsTTL,
		now:           time.Now,
	}
}

// Read returns the organization's state, refreshing whichever halves have
// expired. With both halves fresh this costs nothing; with one expired it
// costs that half's requests only.
func (c *Cache) Read(ctx context.Context) (*State, error) {
	c.mu.Lock()
	membership, teams := c.membership, c.teams
	c.mu.Unlock()

	needMembership := membership == nil || c.now().Sub(membership.FetchedAt) > c.membershipTTL
	needTeams := teams == nil || c.now().Sub(teams.FetchedAt) > c.teamsTTL

	group, groupCtx := errgroup.WithContext(ctx)

	if needMembership {
		group.Go(func() error {
			fresh, err := c.reader.ReadMembership(groupCtx)
			if err != nil {
				return err
			}

			membership = fresh

			return nil
		})
	}

	if needTeams {
		group.Go(func() error {
			fresh, err := c.reader.ReadTeams(groupCtx)
			if err != nil {
				return err
			}

			teams = fresh

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.membership, c.teams = membership, teams
	c.mu.Unlock()

	return assemble(c.org, membership, teams), nil
}

// ReadFresh bypasses the cache entirely, stores what it read, and returns
// it. The sync and removals paths use this: a plan must diff against the
// truth right now, never against a TTL.
func (c *Cache) ReadFresh(ctx context.Context) (*State, error) {
	var (
		membership *Membership
		teams      *TeamState
	)

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		var err error
		membership, err = c.reader.ReadMembership(groupCtx)

		return err
	})

	group.Go(func() error {
		var err error
		teams, err = c.reader.ReadTeams(groupCtx)

		return err
	})

	if err := group.Wait(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.membership, c.teams = membership, teams
	c.mu.Unlock()

	return assemble(c.org, membership, teams), nil
}

// Invalidate drops both halves. Called after every applier run: most
// GitHub state changes go through this service, so after one the cache is
// wrong by construction.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.membership, c.teams = nil, nil
	c.mu.Unlock()
}
