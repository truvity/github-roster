package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// discoveryTimeout bounds the one startup call to the provider. Generous,
// because a slow provider at boot is a bad reason to crash-loop; bounded,
// because hanging forever is worse.
const discoveryTimeout = 15 * time.Second

// newHTTPClient returns the client used for discovery and JWKS fetches.
// stdlib net/http, per the fleet's Go stack canon.
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: discoveryTimeout}
}

// endpoints is what this service needs from the provider's discovery
// document.
type endpoints struct {
	JWKSURI     string
	UserinfoURI string
}

// discover reads the provider's OpenID configuration.
//
// Discovery is done by hand rather than pulled in with an OIDC relying-party
// library: this service is not a relying party. It never runs a code flow,
// holds no client credentials and mints no sessions — it verifies a token
// somebody else obtained and, for display claims the access token does not
// carry, asks the userinfo endpoint. One well-known document and two fields
// is the whole requirement.
func discover(ctx context.Context, issuer string) (*endpoints, error) {
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build discovery request for %q: %w", issuer, err)
	}

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", wellKnown, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %q: unexpected status %s", wellKnown, resp.Status)
	}

	var document struct {
		Issuer      string `json:"issuer"`
		JWKSURI     string `json:"jwks_uri"`
		UserinfoURI string `json:"userinfo_endpoint"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		return nil, fmt.Errorf("decode %q: %w", wellKnown, err)
	}

	// The issuer must match what we were configured with, or a redirect to
	// somebody else's provider would silently hand them the console.
	if document.Issuer != issuer {
		return nil, fmt.Errorf("discovery at %q declares issuer %q, want %q", wellKnown, document.Issuer, issuer)
	}

	if document.JWKSURI == "" {
		return nil, fmt.Errorf("discovery at %q declares no jwks_uri", wellKnown)
	}

	return &endpoints{JWKSURI: document.JWKSURI, UserinfoURI: document.UserinfoURI}, nil
}
