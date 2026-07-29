// Package mappingtest holds the contract every [mapping.Store] must
// satisfy.
//
// It is an ordinary package rather than a _test file so the integration
// suite can run the very same assertions against a real parameter store. A
// fake that behaves differently from the real thing is worse than no fake at
// all, and this is what keeps them honest as the interface grows.
package mappingtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/mapping"
)

// StoreSuite runs the store contract against a factory.
func StoreSuite(t *testing.T, newStore func(t *testing.T) mapping.Store) {
	t.Helper()

	ctx := context.Background()

	t.Run("get on an empty store", func(t *testing.T) {
		store := newStore(t)

		_, err := store.Get(ctx, "Nobody Here")
		require.ErrorIs(t, err, mapping.ErrNotFound)
	})

	t.Run("put then get round trip", func(t *testing.T) {
		store := newStore(t)

		want := mapping.Entry{
			Name:   "A Person",
			GitHub: "octocat",
			K8s:    "aperson",
			Class:  mapping.ClassEmployee,
		}
		require.NoError(t, store.Put(ctx, want))

		got, err := store.Get(ctx, want.Name)
		require.NoError(t, err)

		require.Equal(t, want.Name, got.Name)
		require.Equal(t, want.GitHub, got.GitHub)
		require.Equal(t, want.K8s, got.K8s)
		require.Equal(t, want.Class, got.Class)
		require.NotEmpty(t, got.Revision, "a stored entry should carry a revision")
	})

	t.Run("pinned memberships survive a round trip", func(t *testing.T) {
		store := newStore(t)

		want := mapping.Entry{
			Name:   "A Bot",
			GitHub: "example-bot",
			Class:  mapping.ClassBot,
			Pinned: []string{"example-org/robots", "example-org/auditor"},
		}
		require.NoError(t, store.Put(ctx, want))

		got, err := store.Get(ctx, want.Name)
		require.NoError(t, err)
		require.Equal(t, want.Pinned, got.Pinned)
	})

	t.Run("list returns everything", func(t *testing.T) {
		store := newStore(t)

		require.NoError(t, store.Put(ctx, mapping.Entry{Name: "A Person", GitHub: "octocat", Class: mapping.ClassEmployee}))
		require.NoError(t, store.Put(ctx, mapping.Entry{Name: "B Person", GitHub: "hubot", Class: mapping.ClassEmployee}))

		entries, err := store.List(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 2)

		names := map[string]bool{}
		for _, e := range entries {
			names[e.Name] = true
		}

		require.True(t, names["A Person"] && names["B Person"], "got %v", names)
	})

	t.Run("update replaces the entry", func(t *testing.T) {
		store := newStore(t)

		require.NoError(t, store.Put(ctx, mapping.Entry{Name: "A Person", GitHub: "octocat", Class: mapping.ClassEmployee}))
		require.NoError(t, store.Put(ctx, mapping.Entry{Name: "A Person", GitHub: "octocat", K8s: "aperson", Class: mapping.ClassEmployee}))

		got, err := store.Get(ctx, "A Person")
		require.NoError(t, err)
		require.Equal(t, "aperson", got.K8s)
	})

	// A stale pinned membership would keep granting a team the operator
	// just took away — the store must remove the field, not leave it.
	t.Run("clearing pinned memberships removes them", func(t *testing.T) {
		store := newStore(t)

		pinned := mapping.Entry{Name: "A Bot", GitHub: "example-bot", Class: mapping.ClassBot, Pinned: []string{"example-org/robots"}}
		require.NoError(t, store.Put(ctx, pinned))

		pinned.Pinned = nil
		require.NoError(t, store.Put(ctx, pinned))

		got, err := store.Get(ctx, "A Bot")
		require.NoError(t, err)
		require.Empty(t, got.Pinned)
	})

	t.Run("invariants are enforced by the store, not just the UI", func(t *testing.T) {
		store := newStore(t)

		require.NoError(t, store.Put(ctx, mapping.Entry{Name: "A Person", GitHub: "octocat", K8s: "aperson", Class: mapping.ClassEmployee}))

		err := store.Put(ctx, mapping.Entry{Name: "B Person", GitHub: "hubot", K8s: "aperson", Class: mapping.ClassEmployee})
		require.ErrorIs(t, err, mapping.ErrDuplicate)

		err = store.Put(ctx, mapping.Entry{Name: "C Person", GitHub: "OCTOCAT", Class: mapping.ClassEmployee})
		require.ErrorIs(t, err, mapping.ErrDuplicate)

		err = store.Put(ctx, mapping.Entry{Name: "A Person", GitHub: "octocat", K8s: "renamed", Class: mapping.ClassEmployee})
		require.ErrorIs(t, err, mapping.ErrImmutable)
	})

	t.Run("delete removes the entry", func(t *testing.T) {
		store := newStore(t)

		require.NoError(t, store.Put(ctx, mapping.Entry{Name: "A Person", GitHub: "octocat", Class: mapping.ClassEmployee}))
		require.NoError(t, store.Delete(ctx, "A Person"))

		_, err := store.Get(ctx, "A Person")
		require.ErrorIs(t, err, mapping.ErrNotFound)
	})

	// The caller wanted it gone, and it is gone.
	t.Run("deleting an absent entry is not an error", func(t *testing.T) {
		store := newStore(t)
		require.NoError(t, store.Delete(ctx, "Never Existed"))
	})
}
