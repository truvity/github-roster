package auth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwk"
)

// jwx v4 is transport-agnostic by design: fetching and caching a key set are
// "the implementation's responsibility". The companion fetcher the docs point
// at is a 2022 pseudo-version that pulls jwx v1 into the module graph, which
// is a worse trade in a public repo than these sixty lines.

const (
	// keySetRefresh is how often the key set is refetched — short enough to
	// follow a provider's key rotation without an operator noticing, long
	// enough that the console is not a load generator against the provider.
	keySetRefresh = 15 * time.Minute
	// keySetFetchTimeout bounds one fetch.
	keySetFetchTimeout = 15 * time.Second
	// keySetMaxBytes caps the response. A key set is a few kilobytes; this
	// stops a compromised or confused endpoint from feeding us a stream.
	keySetMaxBytes = 1 << 20
)

// keySet holds the issuer's public keys and refreshes them in the
// background.
//
// A refresh failure is deliberately not fatal: the previous key set stays in
// place and requests keep verifying. The alternative — dropping the keys —
// would turn a blip at the identity provider into an outage of a console
// whose whole job is to still work when things are going wrong.
type keySet struct {
	logger *slog.Logger
	url    string
	client *http.Client

	mu   sync.RWMutex
	keys jwk.Set
}

func newKeySet(ctx context.Context, logger *slog.Logger, url string) (*keySet, error) {
	ks := &keySet{
		logger: logger,
		url:    url,
		client: &http.Client{Timeout: keySetFetchTimeout},
	}

	// Fetch once now: an unreachable JWKS is a startup failure, not a stream
	// of 401s an hour after the rollout.
	if err := ks.refresh(ctx); err != nil {
		return nil, err
	}

	go ks.refreshLoop(ctx)

	return ks, nil
}

// Set returns the current key set, for jwt.WithKeySet.
func (k *keySet) Set() jwk.Set {
	k.mu.RLock()
	defer k.mu.RUnlock()

	return k.keys
}

func (k *keySet) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(keySetRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := k.refresh(ctx); err != nil {
				k.logger.WarnContext(ctx, "JWKS refresh failed; keeping the previous key set",
					slog.String("url", k.url),
					slog.Any("error", err))
			}
		}
	}
}

func (k *keySet) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, keySetFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, http.NoBody)
	if err != nil {
		return fmt.Errorf("build JWKS request for %q: %w", k.url, err)
	}

	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS %q: %w", k.url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS %q: unexpected status %s", k.url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, keySetMaxBytes))
	if err != nil {
		return fmt.Errorf("read JWKS %q: %w", k.url, err)
	}

	set, err := jwk.Parse(body)
	if err != nil {
		return fmt.Errorf("parse JWKS %q: %w", k.url, err)
	}

	if set.Len() == 0 {
		// An empty set verifies nothing, so adopting it would lock everyone
		// out on the next request.
		return fmt.Errorf("JWKS %q contains no keys", k.url)
	}

	k.mu.Lock()
	k.keys = set
	k.mu.Unlock()

	return nil
}
