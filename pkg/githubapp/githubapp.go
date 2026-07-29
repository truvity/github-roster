// Package githubapp authenticates as a GitHub App installation.
//
// A GitHub App does not have a password. It has a private key, which signs a
// short-lived JWT proving "I am this App", which is exchanged for an
// installation token proving "I am this App, acting on this organization".
// The installation token is what every API call actually uses, and it
// expires after an hour.
//
// This is deliberately a small, self-contained package rather than a
// dependency: it is forty lines of well-specified protocol, and a service
// whose whole security argument rests on which credential it holds should
// be able to show exactly how it uses them.
package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jwt"
)

const (
	// appJWTLifetime is how long the App JWT is valid. GitHub rejects
	// anything over ten minutes.
	appJWTLifetime = 9 * time.Minute
	// clockSkew backdates `iat`. GitHub rejects a JWT issued in the future,
	// and a machine whose clock is a few seconds fast is common enough that
	// the day-1 runbook lists it as a cause of "401 with a valid key".
	clockSkew = 30 * time.Second
	// renewBefore refreshes an installation token this long before it
	// expires, so a call never races the expiry.
	renewBefore = 5 * time.Minute

	requestTimeout = 30 * time.Second
	// apiBase is github.com. Kept a field on Credentials so a test can
	// point the whole exchange at a local server.
	apiBase = "https://api.github.com"
)

// Credentials are one App installation's.
type Credentials struct {
	// AppID is the App's numeric ID.
	AppID int64
	// InstallationID identifies the installation on one organization. An
	// App installed on two organizations has two of these, and using the
	// wrong one authenticates against the wrong org.
	InstallationID int64
	// PrivateKey is the PEM exactly as GitHub issued it.
	PrivateKey []byte

	// BaseURL overrides the API root. Empty means github.com.
	BaseURL string
	// HTTPClient overrides the client used for the token exchange.
	HTTPClient *http.Client
}

// TokenSource mints and caches installation tokens.
//
// Safe for concurrent use: the console serves pages in parallel and every
// one of them needs a token.
type TokenSource struct {
	creds  Credentials
	client *http.Client
	base   string

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewTokenSource returns a token source for one installation. The private
// key is parsed once here, so a malformed key fails at startup rather than
// on the first request.
func NewTokenSource(creds Credentials) (*TokenSource, error) {
	if creds.AppID == 0 {
		return nil, fmt.Errorf("app id is required")
	}

	if creds.InstallationID == 0 {
		return nil, fmt.Errorf("installation id is required")
	}

	if _, err := parseKey(creds.PrivateKey); err != nil {
		return nil, err
	}

	client := creds.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	base := creds.BaseURL
	if base == "" {
		base = apiBase
	}

	return &TokenSource{creds: creds, client: client, base: base}, nil
}

// Token returns a valid installation token, minting a new one when the
// cached one is close to expiring.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && time.Now().Before(t.expiresAt.Add(-renewBefore)) {
		return t.token, nil
	}

	token, expiresAt, err := t.mint(ctx)
	if err != nil {
		return "", err
	}

	t.token, t.expiresAt = token, expiresAt

	return token, nil
}

func (t *TokenSource) mint(ctx context.Context) (string, time.Time, error) {
	assertion, err := t.appJWT()
	if err != nil {
		return "", time.Time{}, err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", t.base, t.creds.InstallationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build token request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request installation token: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		// The body can name the problem ("integration not installed"), and
		// it never contains the key.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

		return "", time.Time{}, fmt.Errorf("installation token for app %d installation %d: %s: %s",
			t.creds.AppID, t.creds.InstallationID, resp.Status, body)
	}

	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", time.Time{}, fmt.Errorf("decode installation token: %w", err)
	}

	if payload.Token == "" {
		return "", time.Time{}, fmt.Errorf("installation token response carried no token")
	}

	return payload.Token, payload.ExpiresAt, nil
}

// appJWT mints the short-lived assertion that proves App identity.
func (t *TokenSource) appJWT() (string, error) {
	key, err := parseKey(t.creds.PrivateKey)
	if err != nil {
		return "", err
	}

	now := time.Now()

	token, err := jwt.NewBuilder().
		Issuer(strconv.FormatInt(t.creds.AppID, 10)).
		IssuedAt(now.Add(-clockSkew)).
		Expiration(now.Add(appJWTLifetime)).
		Build()
	if err != nil {
		return "", fmt.Errorf("build app jwt: %w", err)
	}

	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), key))
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}

	return string(signed), nil
}

func parseKey(pem []byte) (jwk.Key, error) {
	if len(pem) == 0 {
		return nil, fmt.Errorf("private key is empty")
	}

	key, err := jwk.ParseKey(pem, jwk.WithX509(true))
	if err != nil {
		// Deliberately does not include the input: it is a private key.
		return nil, fmt.Errorf("parse app private key (expected a PEM): %w", err)
	}

	return key, nil
}
