package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jwt"
	"github.com/neilotoole/slogt/v2"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-roster/pkg/config"
)

// fakeIssuer is an identity provider: a discovery document and a JWKS,
// served over a real listener so the verifier exercises its actual fetch
// path rather than an injected key.
type fakeIssuer struct {
	server *httptest.Server
	signer jwk.Key
	public jwk.Key
	// userinfoCalls counts hits on /userinfo, so tests can prove the
	// display cache holds.
	userinfoCalls int
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()

	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	signer, err := jwk.Import[jwk.Key](raw)
	require.NoError(t, err)
	require.NoError(t, signer.Set(jwk.KeyIDKey, "test-key"))
	require.NoError(t, signer.Set(jwk.AlgorithmKey, jwa.RS256()))

	public, err := jwk.PublicKeyOf(signer)
	require.NoError(t, err)

	issuer := &fakeIssuer{signer: signer, public: public}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":            issuer.server.URL,
			"jwks_uri":          issuer.server.URL + "/keys",
			"userinfo_endpoint": issuer.server.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		issuer.userinfoCalls++

		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sub":   "u-1",
			"name":  "A Person",
			"email": "a@example.com",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		set := jwk.NewSet()
		require.NoError(t, set.AddKey(public))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})

	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)

	return issuer
}

// token mints a signed token. Options mutate it before signing so a test can
// state exactly what is wrong with the token it is about to present.
func (f *fakeIssuer) token(t *testing.T, mutate func(b *jwt.Builder)) string {
	t.Helper()

	builder := jwt.NewBuilder().
		Issuer(f.server.URL).
		Subject("u-1").
		IssuedAt(time.Now()).
		Expiration(time.Now().Add(time.Hour)).
		Claim("name", "A Person").
		Claim("email", "a@example.com").
		Claim("groups", []string{"operators"})

	if mutate != nil {
		mutate(builder)
	}

	token, err := builder.Build()
	require.NoError(t, err)

	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), f.signer))
	require.NoError(t, err)

	return string(signed)
}

func newTestVerifier(t *testing.T, issuer *fakeIssuer, audience string) Authenticator {
	t.Helper()

	authenticator, err := NewVerifier(t.Context(), slogt.New(t), config.OIDC{
		Issuer:     issuer.server.URL,
		Audience:   audience,
		RolesClaim: "groups",
		Roles:      config.Roles{Viewer: "viewers", Operator: "operators"},
	})
	require.NoError(t, err)

	return authenticator
}

// probe runs one request through the middleware and reports the status plus
// the role the handler saw.
func probe(t *testing.T, authenticator Authenticator, header string) (int, Role) {
	t.Helper()

	var seen Role

	app := fiber.New()
	app.Use(authenticator.Middleware())
	app.Get("/", func(c fiber.Ctx) error {
		identity, _ := From(c)
		seen = identity.Role

		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	if header != "" {
		req.Header.Set(fiber.HeaderAuthorization, header)
	}

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode, seen
}

func TestVerifierAcceptsAGoodToken(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	status, role := probe(t, authenticator, "Bearer "+issuer.token(t, nil))

	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, RoleOperator, role)
}

func TestVerifierAcceptsForwardedAccessTokenHeader(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	// A browser session carries no Authorization header; the oauth2-proxy
	// gateway forwards the raw access token as X-Auth-Request-Access-Token.
	app := fiber.New()
	app.Use(authenticator.Middleware())

	var seen Role

	app.Get("/", func(c fiber.Ctx) error {
		identity, _ := From(c)
		seen = identity.Role

		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("X-Auth-Request-Access-Token", issuer.token(t, nil)) // no "Bearer " prefix

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, RoleOperator, seen)
}

func TestVerifierRejects(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	other := newFakeIssuer(t)

	cases := []struct {
		name   string
		header func(t *testing.T) string
		status int
	}{
		{
			name:   "no header at all",
			header: func(*testing.T) string { return "" },
			status: fiber.StatusUnauthorized,
		},
		{
			name:   "not a token",
			header: func(*testing.T) string { return "Bearer not-a-jwt" },
			status: fiber.StatusUnauthorized,
		},
		{
			// The point of verifying rather than trusting: a token minted
			// by anyone else must not open the console.
			name:   "signed by a different issuer",
			header: func(t *testing.T) string { return "Bearer " + other.token(t, nil) },
			status: fiber.StatusUnauthorized,
		},
		{
			name: "expired",
			header: func(t *testing.T) string {
				return "Bearer " + issuer.token(t, func(b *jwt.Builder) {
					b.Expiration(time.Now().Add(-time.Minute))
				})
			},
			status: fiber.StatusUnauthorized,
		},
		{
			// Claims the right issuer string but is signed with the wrong
			// key — the substitution a stolen-and-edited token would make.
			name: "issuer claim forged",
			header: func(t *testing.T) string {
				return "Bearer " + other.token(t, func(b *jwt.Builder) {
					b.Issuer(issuer.server.URL)
				})
			},
			status: fiber.StatusUnauthorized,
		},
		{
			// Authenticated, but in none of the console's groups: a 403,
			// because re-authenticating cannot help them.
			name: "valid token, no console group",
			header: func(t *testing.T) string {
				return "Bearer " + issuer.token(t, func(b *jwt.Builder) {
					b.Claim("groups", []string{"everyone"})
				})
			},
			status: fiber.StatusForbidden,
		},
	}

	authenticator := newTestVerifier(t, issuer, "")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, role := probe(t, authenticator, tc.header(t))

			require.Equal(t, tc.status, status)
			require.Equal(t, RoleNone, role, "a rejected caller must not reach the handler")
		})
	}
}

func TestVerifierChecksAudienceWhenConfigured(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "roster")

	wrong := issuer.token(t, func(b *jwt.Builder) { b.Audience([]string{"something-else"}) })
	status, _ := probe(t, authenticator, "Bearer "+wrong)
	require.Equal(t, fiber.StatusUnauthorized, status)

	right := issuer.token(t, func(b *jwt.Builder) { b.Audience([]string{"roster"}) })
	status, role := probe(t, authenticator, "Bearer "+right)
	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, RoleOperator, role)
}

// A single group arriving as a bare string is common enough that treating it
// as "no groups" would lock out real people.
func TestVerifierAcceptsAScalarRolesClaim(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	token := issuer.token(t, func(b *jwt.Builder) { b.Claim("groups", "viewers") })

	status, role := probe(t, authenticator, "Bearer "+token)
	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, RoleViewer, role)
}

// probeIdentity runs one request through the middleware with an optional
// ID-token cookie and reports the full identity the handler saw.
func probeIdentity(t *testing.T, authenticator Authenticator, header, cookie string) (int, Identity) {
	t.Helper()

	var seen Identity

	app := fiber.New()
	app.Use(authenticator.Middleware())
	app.Get("/", func(c fiber.Ctx) error {
		seen, _ = From(c)

		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, header)

	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: idTokenCookie, Value: cookie})
	}

	resp, err := app.Test(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode, seen
}

// The gateway's access token carries roles but no profile claims — those are
// asserted into the ID token, which the gateway stores in a cookie. The
// display name must come from that cookie, verified, and only for the same
// subject.
func TestDisplayClaimsFallBackToTheIDTokenCookie(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	access := issuer.token(t, func(b *jwt.Builder) {
		b.Claim("name", "").Claim("email", "")
	})
	idToken := issuer.token(t, nil)

	status, identity := probeIdentity(t, authenticator, "Bearer "+access, idToken)
	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, "A Person", identity.Name)
	require.Equal(t, "a@example.com", identity.Email)
	require.Equal(t, RoleOperator, identity.Role)
}

// A cookie for a different subject is somebody else's session leftovers; its
// claims must never be displayed as the caller's — the verifier falls
// through to userinfo instead.
func TestDisplayClaimsIgnoreAForeignIDToken(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	access := issuer.token(t, func(b *jwt.Builder) {
		b.Claim("name", "").Claim("email", "")
	})
	foreign := issuer.token(t, func(b *jwt.Builder) {
		b.Subject("u-2").Claim("name", "Someone Else")
	})

	status, identity := probeIdentity(t, authenticator, "Bearer "+access, foreign)
	require.Equal(t, fiber.StatusOK, status)
	require.NotEqual(t, "Someone Else", identity.Name,
		"the foreign cookie's claims must not be displayed as the caller's")
	require.Equal(t, "A Person", identity.Name, "userinfo answers instead")
	require.Equal(t, 1, issuer.userinfoCalls)
}

// A garbage cookie must not break the request — the access token alone
// already authenticated the caller, and userinfo covers the display.
func TestDisplayClaimsSurviveAGarbageCookie(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	access := issuer.token(t, func(b *jwt.Builder) {
		b.Claim("name", "").Claim("email", "")
	})

	status, identity := probeIdentity(t, authenticator, "Bearer "+access, "not-a-jwt")
	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, RoleOperator, identity.Role)
	require.Equal(t, "A Person", identity.Name, "userinfo answers despite the bad cookie")
}

// When neither the access token nor any cookie carries display claims, the
// verifier asks the issuer's userinfo endpoint with the caller's own access
// token — and caches the answer per subject.
func TestDisplayClaimsFallBackToUserinfo(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	access := issuer.token(t, func(b *jwt.Builder) {
		b.Claim("name", "").Claim("email", "")
	})

	status, identity := probeIdentity(t, authenticator, "Bearer "+access, "")
	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, "A Person", identity.Name)
	require.Equal(t, "a@example.com", identity.Email)
	require.Equal(t, RoleOperator, identity.Role)
	require.Equal(t, 1, issuer.userinfoCalls)

	// The second request is served from the display cache.
	status, identity = probeIdentity(t, authenticator, "Bearer "+access, "")
	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, "A Person", identity.Name)
	require.Equal(t, 1, issuer.userinfoCalls)
}

// A usable ID-token cookie wins without any userinfo round-trip.
func TestUserinfoIsNotAskedWhenTheCookieAnswers(t *testing.T) {
	t.Parallel()

	issuer := newFakeIssuer(t)
	authenticator := newTestVerifier(t, issuer, "")

	access := issuer.token(t, func(b *jwt.Builder) {
		b.Claim("name", "").Claim("email", "")
	})
	idToken := issuer.token(t, nil)

	status, identity := probeIdentity(t, authenticator, "Bearer "+access, idToken)
	require.Equal(t, fiber.StatusOK, status)
	require.Equal(t, "A Person", identity.Name)
	require.Equal(t, 0, issuer.userinfoCalls)
}

// Discovery that names a different issuer than the one configured means a
// redirect landed us at somebody else's provider.
func TestDiscoveryIssuerMismatchIsFatal(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   "https://somebody.else",
			"jwks_uri": "https://somebody.else/keys",
		})
	}))
	t.Cleanup(server.Close)

	_, err := NewVerifier(context.Background(), slogt.New(t), config.OIDC{
		Issuer:     server.URL,
		RolesClaim: "groups",
		Roles:      config.Roles{Viewer: "viewers", Operator: "operators"},
	})

	require.ErrorContains(t, err, "declares issuer")
}
