package directory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/neilotoole/slogt/v2"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/directory"
)

func TestFullName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                 string
		given, family, whole string
		want                 string
	}{
		{"both parts", "Ada", "Lovelace", "Ada Lovelace", "Ada Lovelace"},
		// The parts win over the full name, which is free text people edit
		// into shapes the join will never match.
		{"parts beat a decorated full name", "Robert", "Smith", "Bob (Robert) Smith", "Robert Smith"},
		{"parts are trimmed", "  Ada  ", " Lovelace ", "", "Ada Lovelace"},
		{"only a full name", "", "", "Ada Lovelace", "Ada Lovelace"},
		{"only a given name", "Ada", "", "", "Ada"},
		{"nothing at all", "", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, directory.FullName(tc.given, tc.family, tc.whole))
		})
	}
}

func snapshot() *directory.Snapshot {
	return &directory.Snapshot{
		Source: "corp",
		Users: []directory.User{
			{Name: "Ada Lovelace", Email: "ada@example.com", Live: true},
			{Name: "Alan Turing", Email: "alan@example.com", Live: true},
			{Name: "Gone Person", Email: "gone@example.com", Live: false},
		},
		Groups: map[string][]string{
			"engineers@example.com": {"ada@example.com", "alan@example.com"},
			"sre@example.com":       {"alan@example.com", "gone@example.com"},
		},
		FetchedAt: time.Now(),
	}
}

func TestLiveUsersExcludesSuspended(t *testing.T) {
	t.Parallel()

	live := snapshot().LiveUsers()
	require.Len(t, live, 2)

	for _, u := range live {
		require.NotEqual(t, "Gone Person", u.Name)
	}
}

// A team drawn from two groups means "anyone in either" — the union, which
// is how people describe them.
func TestGroupMembersUnionsAndDeduplicates(t *testing.T) {
	t.Parallel()

	members := snapshot().GroupMembers([]string{"engineers@example.com", "sre@example.com"})

	require.ElementsMatch(t, []string{"ada@example.com", "alan@example.com", "gone@example.com"}, members)
}

func TestGroupMembersIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	require.ElementsMatch(t,
		[]string{"ada@example.com", "alan@example.com"},
		snapshot().GroupMembers([]string{"Engineers@Example.COM"}))
}

// An unknown group is empty rather than an error: it means nobody is in it
// yet, and failing would take the whole page down over a typo.
func TestGroupMembersOfAnUnknownGroupIsEmpty(t *testing.T) {
	t.Parallel()

	require.Empty(t, snapshot().GroupMembers([]string{"nobody@example.com"}))
}

func TestUserByEmail(t *testing.T) {
	t.Parallel()

	s := snapshot()

	user, ok := s.UserByEmail("ADA@EXAMPLE.COM")
	require.True(t, ok)
	require.Equal(t, "Ada Lovelace", user.Name)

	_, ok = s.UserByEmail("nobody@example.com")
	require.False(t, ok)
}

// fakeSource returns whatever the test tells it to.
type fakeSource struct {
	name string
	snap *directory.Snapshot
	err  error
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Fetch(context.Context) (*directory.Snapshot, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.snap, nil
}

// The central safety property: a directory that fails must not read as
// everybody having left.
func TestCacheKeepsLastKnownGoodOnFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	source := &fakeSource{name: "corp", snap: snapshot()}
	cache := directory.NewCache(slogt.New(t), source)

	require.NoError(t, cache.Refresh(ctx))

	status := cache.Status()
	require.True(t, status.Healthy)
	require.True(t, status.Ready)

	// Now the directory breaks.
	source.err = errors.New("directory unreachable")
	require.Error(t, cache.Refresh(ctx))

	snap, ok := cache.Snapshot()
	require.True(t, ok, "the previous snapshot must survive a failed fetch")
	require.Len(t, snap.Users, 3, "and it must still hold the people it held before")

	status = cache.Status()
	require.False(t, status.Healthy, "the source must be reported unhealthy")
	require.True(t, status.Ready, "but it is still usable for display")
	require.Contains(t, status.Error, "directory unreachable")
}

// Before the first successful fetch there is nothing to act on, and that is
// a different state from "fetched, and empty".
func TestCacheIsNotReadyBeforeItsFirstFetch(t *testing.T) {
	t.Parallel()

	cache := directory.NewCache(slogt.New(t), &fakeSource{name: "corp", err: errors.New("nope")})

	require.Error(t, cache.Refresh(context.Background()))

	_, ok := cache.Snapshot()
	require.False(t, ok)

	status := cache.Status()
	require.False(t, status.Ready)
	require.False(t, status.Healthy)
}

// One broken directory must not stop the others being read.
func TestSetRefreshesEverySourceDespiteFailures(t *testing.T) {
	t.Parallel()

	good := &fakeSource{name: "good", snap: snapshot()}
	bad := &fakeSource{name: "bad", err: errors.New("boom")}
	alsoGood := &fakeSource{name: "also-good", snap: snapshot()}

	set := directory.NewSet(slogt.New(t), good, bad, alsoGood)

	errs := set.Refresh(context.Background())

	require.Len(t, errs, 1)
	require.Contains(t, errs, "bad")

	require.Equal(t, []string{"bad"}, set.Unhealthy(),
		"only the broken source may have its removals skipped")

	for _, cache := range set.Caches() {
		if cache.Name() == "bad" {
			continue
		}

		_, ok := cache.Snapshot()
		require.True(t, ok, "source %q should have been read", cache.Name())
	}
}

func TestSetStatuses(t *testing.T) {
	t.Parallel()

	set := directory.NewSet(slogt.New(t),
		&fakeSource{name: "a", snap: snapshot()},
		&fakeSource{name: "b", err: errors.New("boom")})

	set.Refresh(context.Background())

	statuses := set.Statuses()
	require.Len(t, statuses, 2)

	byName := map[string]directory.Status{}
	for _, s := range statuses {
		byName[s.Source] = s
	}

	require.True(t, byName["a"].Healthy)
	require.False(t, byName["b"].Healthy)
	require.NotZero(t, byName["a"].FetchedAt)
}
