package directory

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
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
		// Rounded: this renders on the console, and nanosecond precision
		// reads as noise ("5m6.442486603s ago").
		status.Age = time.Since(c.snapshot.FetchedAt).Round(time.Second)
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

// staleAt reports whether a background refresh is warranted at now: the
// snapshot is missing or older than ttl, and the last attempt — success or
// failure — is older than retryBackoff. The backoff is what keeps a broken
// source from being retried on every page view: without it, a directory
// that answers with an error in 50ms would be hit once per render.
func (c *Cache) staleAt(now time.Time, ttl, retryBackoff time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.snapshot != nil && now.Sub(c.snapshot.FetchedAt) <= ttl {
		return false
	}

	return now.Sub(c.lastTry) >= retryBackoff
}

// refreshTimeout bounds one background refresh sweep. A real fetch takes a
// few seconds per source; a source that cannot answer within this is the
// failure case the cache exists for.
const refreshTimeout = 2 * time.Minute

// Set is the group of caches the service reads, one per configured source.
type Set struct {
	caches []*Cache

	// refreshing is the single-flight guard for RefreshIfStale: many
	// renders may notice staleness at once, one sweep runs.
	refreshing atomic.Bool
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

// RefreshIfStale starts one background refresh of every source whose
// snapshot is older than ttl, provided that source has not been attempted
// within retryBackoff. It reports whether a refresh was started.
//
// It never blocks: the caller is a page render, and a page must serve the
// cache it has rather than wait on a directory (the next view sees the
// fresh read). The refresh runs on a detached context — deliberately not
// the request's, which dies with the response and whose fasthttp backing
// is recycled.
//
// This is display freshness only. Anything that mutates refreshes its
// sources synchronously at plan time and does not rely on this.
func (s *Set) RefreshIfStale(ttl, retryBackoff time.Duration) bool {
	now := time.Now()

	var stale []*Cache

	for _, cache := range s.caches {
		if cache.staleAt(now, ttl, retryBackoff) {
			stale = append(stale, cache)
		}
	}

	if len(stale) == 0 {
		return false
	}

	if !s.refreshing.CompareAndSwap(false, true) {
		return false
	}

	go func() {
		defer s.refreshing.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()

		// The error is recorded in the cache and logged by Refresh;
		// failures update lastTry, which arms the backoff for the next
		// render.
		for _, cache := range stale {
			_ = cache.Refresh(ctx)
		}
	}()

	return true
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
