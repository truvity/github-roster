package directory

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Cache holds the last successful read of one source.
//
// This is where the design's most important failure rule lives: **a failed
// fetch must never look like everybody leaving**. A directory that times out
// returns no users, and a naive reader would conclude the company is empty
// and revoke everyone. So a failure keeps the previous snapshot, records the
// error, and lets the caller decide — and the caller's rule is to skip that
// source's removals entirely until it is healthy again.
//
// Staleness is surfaced rather than hidden. An operator looking at the
// console needs to know they are reading yesterday's directory, because the
// alternative is confidently acting on it.
type Cache struct {
	source Source
	logger *slog.Logger

	mu       sync.RWMutex
	snapshot *Snapshot
	lastErr  error
	lastTry  time.Time
}

// NewCache wraps a source.
func NewCache(logger *slog.Logger, source Source) *Cache {
	return &Cache{source: source, logger: logger}
}

// Name returns the wrapped source's name.
func (c *Cache) Name() string { return c.source.Name() }

// Refresh fetches and, on success, replaces the cached snapshot.
//
// On failure the previous snapshot is kept and the error is both returned
// and recorded. Callers that refresh every source in a loop should NOT stop
// at the first error: one broken directory must not stop the others being
// read.
func (c *Cache) Refresh(ctx context.Context) error {
	snapshot, err := c.source.Fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastTry = time.Now()

	if err != nil {
		c.lastErr = err

		c.logger.WarnContext(ctx, "directory fetch failed; keeping the last known good snapshot",
			slog.String("source", c.source.Name()),
			slog.Bool("have_previous", c.snapshot != nil),
			slog.Any("error", err))

		return err
	}

	c.snapshot = snapshot
	c.lastErr = nil

	c.logger.InfoContext(ctx, "directory fetched",
		slog.String("source", c.source.Name()),
		slog.Int("users", len(snapshot.Users)),
		slog.Int("live", len(snapshot.LiveUsers())),
		slog.Int("groups", len(snapshot.Groups)))

	return nil
}

// Status is what the UI shows about one source.
type Status struct {
	Source string `json:"source"`
	// Healthy is false when the last attempt failed, even if a usable
	// snapshot is still cached.
	Healthy bool `json:"healthy"`
	// Ready is false when no successful fetch has ever happened, so there
	// is nothing to act on at all.
	Ready bool `json:"ready"`
	// FetchedAt is when the cached snapshot was read; zero when never.
	FetchedAt time.Time `json:"fetchedAt,omitempty"`
	// Age is how old the cached snapshot is.
	Age time.Duration `json:"age,omitempty"`
	// Error is the last failure, for display. Empty when healthy.
	Error string `json:"error,omitempty"`
}

// Status reports the source's health for the UI and for the guardrails.
func (c *Cache) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := Status{
		Source:  c.source.Name(),
		Healthy: c.lastErr == nil && c.snapshot != nil,
		Ready:   c.snapshot != nil,
	}

	if c.snapshot != nil {
		status.FetchedAt = c.snapshot.FetchedAt
		status.Age = time.Since(c.snapshot.FetchedAt)
	}

	if c.lastErr != nil {
		status.Error = c.lastErr.Error()
	}

	return status
}

// Snapshot returns the last good read, and whether there is one.
//
// It returns a snapshot even when the last fetch failed — stale data is
// still the truth as of some moment, and the caller decides what it is
// allowed to do with it. What the caller must not do is treat "no snapshot"
// as "no people".
func (c *Cache) Snapshot() (*Snapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.snapshot, c.snapshot != nil
}

// Set injects a snapshot directly. Used by tests and by fixture-driven runs
// — the integration suite replaces real directories with fixtures, because
// reading a corporate directory from CI would mean holding directory
// credentials in CI.
func (c *Cache) Set(snapshot *Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.snapshot = snapshot
	c.lastErr = nil
	c.lastTry = time.Now()
}

// Set is the group of caches the service reads, one per configured source.
type Set struct {
	caches []*Cache
}

// NewSet builds a set from sources.
func NewSet(logger *slog.Logger, sources ...Source) *Set {
	set := &Set{caches: make([]*Cache, 0, len(sources))}
	for _, source := range sources {
		set.caches = append(set.caches, NewCache(logger, source))
	}

	return set
}

// Caches exposes the individual caches.
func (s *Set) Caches() []*Cache { return s.caches }

// Refresh refreshes every source, and does not stop at the first failure:
// one broken directory must not prevent the others being read. It returns
// the errors it collected, keyed by source name.
func (s *Set) Refresh(ctx context.Context) map[string]error {
	errs := map[string]error{}

	for _, cache := range s.caches {
		if err := cache.Refresh(ctx); err != nil {
			errs[cache.Name()] = err
		}
	}

	return errs
}

// Statuses reports every source's health.
func (s *Set) Statuses() []Status {
	statuses := make([]Status, 0, len(s.caches))
	for _, cache := range s.caches {
		statuses = append(statuses, cache.Status())
	}

	return statuses
}

// Unhealthy names the sources whose last fetch failed.
//
// This is the list the removals-only run consults: a source in here has its
// removals skipped, because its absence of data is not evidence of absence
// of people.
func (s *Set) Unhealthy() []string {
	var names []string

	for _, cache := range s.caches {
		if status := cache.Status(); !status.Healthy {
			names = append(names, status.Source)
		}
	}

	return names
}
