package mapping

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"sync"
)

// Compile-time interface check.
var _ Store = (*Memory)(nil)

// Memory is an in-process store.
//
// It exists for local runs and for tests of everything layered above the
// store, and it enforces the same invariants as the real one — a fake that
// accepts what production rejects tests the wrong system.
type Memory struct {
	mu      sync.RWMutex
	entries map[string]Entry
	// revision is a monotonic counter standing in for a real store's
	// version, so callers that display or compare revisions have something
	// to work with here too.
	revision int
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{entries: make(map[string]Entry)}
}

// List returns every entry, sorted by name so tests and pages are stable.
func (m *Memory) List(context.Context) ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := slices.Collect(maps.Values(m.entries))
	slices.SortFunc(entries, sortName)

	return entries, nil
}

// Get returns one entry.
func (m *Memory) Get(_ context.Context, name string) (Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.entries[name]
	if !ok {
		return Entry{}, ErrNotFound
	}

	return entry, nil
}

// Put creates or replaces an entry, enforcing the invariants.
func (m *Memory) Put(_ context.Context, entry Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.entries[entry.Name]

	if err := CheckInvariants(entry, existing, slices.Collect(maps.Values(m.entries))); err != nil {
		return err
	}

	m.revision++
	entry.Revision = strconv.Itoa(m.revision)
	m.entries[entry.Name] = entry

	return nil
}

// Delete removes an entry; removing an absent one is not an error.
func (m *Memory) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.entries, name)

	return nil
}

func sortName(a, b Entry) int {
	switch {
	case a.Name < b.Name:
		return -1
	case a.Name > b.Name:
		return 1
	default:
		return 0
	}
}
