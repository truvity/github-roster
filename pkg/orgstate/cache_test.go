package orgstate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// countingReader serves canned halves and counts how often each is read.
type countingReader struct {
	memberships int
	teamReads   int
	now         func() time.Time
}

func (f *countingReader) ReadMembership(context.Context) (*Membership, error) {
	f.memberships++

	return &Membership{
		Members:   []Member{{Login: "alice", Role: RoleMember}},
		FetchedAt: f.now(),
	}, nil
}

func (f *countingReader) ReadTeams(context.Context) (*TeamState, error) {
	f.teamReads++

	return &TeamState{
		Teams:       []Team{{Slug: "team-x"}},
		TeamMembers: map[string][]string{"team-x": {"alice"}},
		FetchedAt:   f.now(),
	}, nil
}

func newTestCache() (*Cache, *countingReader, *time.Time) {
	clock := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	reader := &countingReader{now: now}

	cache := NewCache("example", reader)
	cache.now = now

	return cache, reader, &clock
}

func TestCacheServesWithinTTLWithoutRereading(t *testing.T) {
	t.Parallel()

	cache, reader, _ := newTestCache()

	first, err := cache.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, "alice", first.Members[0].Login)

	second, err := cache.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.ReadAt, second.ReadAt)

	require.Equal(t, 1, reader.memberships)
	require.Equal(t, 1, reader.teamReads)
}

// The halves expire independently: past the membership TTL but inside the
// teams TTL, only the membership half is re-read.
func TestCacheRefreshesOnlyTheExpiredHalf(t *testing.T) {
	t.Parallel()

	cache, reader, clock := newTestCache()

	_, err := cache.Read(t.Context())
	require.NoError(t, err)

	*clock = clock.Add(DefaultMembershipTTL + time.Second)

	_, err = cache.Read(t.Context())
	require.NoError(t, err)

	require.Equal(t, 2, reader.memberships)
	require.Equal(t, 1, reader.teamReads)

	*clock = clock.Add(DefaultTeamsTTL)

	_, err = cache.Read(t.Context())
	require.NoError(t, err)

	require.Equal(t, 3, reader.memberships)
	require.Equal(t, 2, reader.teamReads)
}

func TestCacheReadFreshAlwaysReads(t *testing.T) {
	t.Parallel()

	cache, reader, _ := newTestCache()

	_, err := cache.Read(t.Context())
	require.NoError(t, err)

	_, err = cache.ReadFresh(t.Context())
	require.NoError(t, err)

	require.Equal(t, 2, reader.memberships)
	require.Equal(t, 2, reader.teamReads)

	// ...and what it read refills the cache.
	_, err = cache.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, reader.memberships)
}

func TestCacheInvalidateDropsBothHalves(t *testing.T) {
	t.Parallel()

	cache, reader, _ := newTestCache()

	_, err := cache.Read(t.Context())
	require.NoError(t, err)

	cache.Invalidate()

	_, err = cache.Read(t.Context())
	require.NoError(t, err)

	require.Equal(t, 2, reader.memberships)
	require.Equal(t, 2, reader.teamReads)
}

// ReadAt must be the OLDER half's fetch time — the honest "as of".
func TestCacheReadAtIsTheOlderHalf(t *testing.T) {
	t.Parallel()

	cache, reader, clock := newTestCache()

	_, err := cache.Read(t.Context())
	require.NoError(t, err)

	teamsFetchedAt := *clock

	*clock = clock.Add(DefaultMembershipTTL + time.Second)

	state, err := cache.Read(t.Context())
	require.NoError(t, err)

	require.Equal(t, 2, reader.memberships)
	require.Equal(t, teamsFetchedAt, state.ReadAt,
		"the refreshed membership must not make stale teams look current")
}
