// Package auth turns a request into a Role.
//
// It deliberately does very little. The login itself — the authorization
// code flow, the session cookie, the redirect dance — belongs to the gateway
// in front of this service (Envoy Gateway's SecurityPolicy `oidc` block),
// which also re-validates the forwarded token against the issuer's JWKS and
// denies anyone outside the console's groups before a request arrives here.
//
// What is left is the part a gateway cannot do on this service's behalf:
// deciding whether this caller is a viewer or an operator, because that
// answer changes which affordances render and which handlers refuse.
//
// The forwarded token is verified again here rather than trusted. Not
// because the gateway is untrustworthy, but because "we are only reachable
// through the gateway" is a property of a NetworkPolicy — one YAML file away
// from being wrong — and a service that can remove people's GitHub access
// should not rest its authorization on that.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/lestrrat-go/jwx/v4/jwt"

	"github.com/truvity/github-roster/pkg/config"
)

// Role is what a caller may do.
type Role string

const (
	// RoleNone is a caller in none of the console's groups.
	RoleNone Role = ""
	// RoleViewer may see structure.
	RoleViewer Role = "viewer"
	// RoleOperator may additionally read the audit log, edit the mapping
	// and trigger a sync.
	RoleOperator Role = "operator"
)

// CanOperate reports whether the role may change anything.
func (r Role) CanOperate() bool { return r == RoleOperator }

// CanView reports whether the role may see the console at all.
func (r Role) CanView() bool { return r == RoleViewer || r == RoleOperator }

// Identity is who the caller is, as far as this service is concerned.
type Identity struct {
	Subject string
	Name    string
	Email   string
	Role    Role
}

// ContextKey is where the identity is parked on the request context.
//
// Fiber's locals are keyed by any, and a package-private struct type is the
// one key another package cannot collide with by accident.
type ContextKey struct{}

// Authenticator resolves a request to an Identity.
type Authenticator interface {
	// Middleware attaches the caller's identity and rejects callers who
	// have none.
	Middleware() fiber.Handler
	// Enabled reports whether tokens are actually being verified.
	Enabled() bool
}

// FromContext returns the identity attached by the middleware.
func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(ContextKey{}).(Identity)

	return identity, ok
}

// WithIdentity attaches an identity to a context.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, ContextKey{}, identity)
}

// From returns the identity attached to a fiber request.
func From(c fiber.Ctx) (Identity, bool) {
	identity, ok := c.Locals(ContextKey{}).(Identity)

	return identity, ok
}

// ---------------------------------------------------------------------------
// Disabled
// ---------------------------------------------------------------------------

// disabled treats every caller as an operator, for local development and
// tests. It is reachable only through an explicit `oidc.disabled: true` —
// never as a fallback from a misconfiguration.
type disabled struct{}

// NewDisabled returns an authenticator that verifies nothing and authorizes
// everybody.
func NewDisabled(logger *slog.Logger) Authenticator {
	logger.Warn("token verification is DISABLED: every caller is treated as an operator")

	return &disabled{}
}

func (d *disabled) Enabled() bool { return false }

func (d *disabled) Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Locals(ContextKey{}, Identity{Subject: "local", Name: "local operator", Role: RoleOperator})

		return c.Next()
	}
}

// ---------------------------------------------------------------------------
// Bearer-token verification
// ---------------------------------------------------------------------------

// verifier reads the JWT the gateway forwards and maps its claims to a role.
type verifier struct {
	logger      *slog.Logger
	keys        *keySet
	issuer      string
	audience    string
	rolesClaim  string
	roles       config.Roles
	userinfoURI string

	// display caches userinfo answers by subject, so the endpoint is
	// asked once per person per TTL rather than once per page.
	displayMu sync.Mutex
	display   map[string]displayClaims
	now       func() time.Time
}

type displayClaims struct {
	Name      string
	Email     string
	FetchedAt time.Time
}

// displayTTL bounds how long a userinfo answer is reused. Names change
// rarely; an hour keeps the endpoint out of the request path without
// making a rename invisible for the workday.
const displayTTL = time.Hour

// NewVerifier builds an authenticator against the issuer's JWKS. Discovery
// and the first key fetch happen here, so an unreachable or misconfigured
// issuer fails at startup rather than on the first request.
func NewVerifier(ctx context.Context, logger *slog.Logger, cfg config.OIDC) (Authenticator, error) {
	found, err := discover(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}

	keys, err := newKeySet(ctx, logger, found.JWKSURI)
	if err != nil {
		return nil, err
	}

	return &verifier{
		logger:      logger,
		keys:        keys,
		issuer:      cfg.Issuer,
		audience:    cfg.Audience,
		rolesClaim:  cfg.RolesClaim,
		roles:       cfg.Roles,
		userinfoURI: found.UserinfoURI,
		display:     map[string]displayClaims{},
		now:         time.Now,
	}, nil
}

func (v *verifier) Enabled() bool { return true }

func (v *verifier) Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		raw, ok := bearerToken(c.Get(fiber.HeaderAuthorization))
		if !ok {
			// A browser session carries no Authorization header: the
			// oauth2-proxy gateway forwards the validated access token as
			// X-Auth-Request-Access-Token (raw, no "Bearer " prefix) — the
			// same token Envoy's jwt_authn validates. Accept it so the
			// console UI (cookie session) authenticates, while direct API
			// callers still use Authorization: Bearer.
			if t := strings.TrimSpace(c.Get(headerAccessToken)); t != "" {
				raw, ok = t, true
			}
		}

		if !ok {
			// The gateway forwards a token on every authenticated request,
			// so a missing one means the request did not come through it.
			return fiber.NewError(fiber.StatusUnauthorized,
				"no bearer token: this service is reached through the gateway")
		}

		token, err := v.parse(raw)
		if err != nil {
			// The reason goes to the log, not to the caller: telling an
			// unauthenticated client why its token failed helps it produce
			// a better one.
			v.logger.WarnContext(c.Context(), "token rejected", slog.Any("error", err))

			return fiber.NewError(fiber.StatusUnauthorized, "invalid token")
		}

		identity := v.identityFrom(token)

		if identity.Name == "" && identity.Email == "" {
			v.fillDisplayClaims(c, &identity)
		}

		if identity.Name == "" && identity.Email == "" {
			// The ID-token cookie was absent or unusable — browsers drop
			// cookies past ~4 KiB, and an ID token with userinfo and role
			// assertions can exceed that. Ask the issuer instead, with
			// the access token that already proved itself.
			v.fillFromUserinfo(c, raw, &identity)
		}

		if !identity.Role.CanView() {
			// The gateway's authorization rules should have stopped this
			// already; reaching here means they and this service's config
			// disagree about which groups matter.
			v.logger.WarnContext(c.Context(), "caller has no console role",
				slog.String("subject", identity.Subject))

			return fiber.NewError(fiber.StatusForbidden,
				"your account is in none of this console's groups")
		}

		c.Locals(ContextKey{}, identity)

		return c.Next()
	}
}

// idTokenCookie is where the gateway's OIDC filter stores the ID token
// (Envoy Gateway's default cookie naming).
const idTokenCookie = "IdToken"

// fillDisplayClaims reads name and email from the gateway's ID-token cookie.
//
// Zitadel asserts profile claims into the ID token, not into the JWT access
// token, so a header rendered from the access token alone would show the
// bare subject ID. The cookie is verified against the same issuer and keys
// before anything is read from it, and it must belong to the same subject as
// the access token. The role deliberately never comes from here.
func (v *verifier) fillDisplayClaims(c fiber.Ctx, identity *Identity) {
	raw := c.Cookies(idTokenCookie)
	if raw == "" {
		return
	}

	token, err := v.parse(raw)
	if err != nil {
		v.logger.DebugContext(c.Context(), "id token cookie rejected", slog.Any("error", err))

		return
	}

	if subject, _ := token.Subject(); subject != identity.Subject {
		return
	}

	identity.Name = stringClaim(token, "name")
	identity.Email = stringClaim(token, "email")
}

// fillFromUserinfo asks the issuer's userinfo endpoint who the caller is,
// authenticating with the caller's own already-verified access token.
// Answers are cached per subject: once an hour per person, not once a page.
// Failures only log — display claims are never worth failing a request.
func (v *verifier) fillFromUserinfo(c fiber.Ctx, accessToken string, identity *Identity) {
	if v.userinfoURI == "" {
		return
	}

	v.displayMu.Lock()
	cached, ok := v.display[identity.Subject]
	v.displayMu.Unlock()

	if ok && v.now().Sub(cached.FetchedAt) < displayTTL {
		identity.Name, identity.Email = cached.Name, cached.Email

		return
	}

	req, err := http.NewRequestWithContext(c.Context(), http.MethodGet, v.userinfoURI, http.NoBody)
	if err != nil {
		return
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		v.logger.WarnContext(c.Context(), "userinfo request failed", slog.Any("error", err))

		return
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		v.logger.WarnContext(c.Context(), "userinfo refused", slog.String("status", resp.Status))

		return
	}

	var body struct {
		Sub   string `json:"sub"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		v.logger.WarnContext(c.Context(), "userinfo undecodable", slog.Any("error", err))

		return
	}

	// The endpoint answered for whoever the token belongs to; require that
	// to be the caller before displaying anything.
	if body.Sub != identity.Subject {
		return
	}

	identity.Name, identity.Email = body.Name, body.Email

	v.displayMu.Lock()
	v.display[identity.Subject] = displayClaims{Name: body.Name, Email: body.Email, FetchedAt: v.now()}
	v.displayMu.Unlock()
}

// parse validates the token's signature, issuer, audience and expiry.
func (v *verifier) parse(raw string) (jwt.Token, error) {
	opts := []jwt.ParseOption{
		jwt.WithKeySet(v.keys.Set()),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.issuer),
	}

	// An empty audience accepts any token this issuer minted. Behind a
	// gateway that already checked the audience this is the right default,
	// and it avoids naming the wrong value on providers whose access tokens
	// carry a project rather than a client.
	if v.audience != "" {
		opts = append(opts, jwt.WithAudience(v.audience))
	}

	token, err := jwt.ParseString(raw, opts...)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}

	return token, nil
}

// headerAccessToken is the header oauth2-proxy sets (pass_access_token) with
// the raw access token — no "Bearer " prefix. Envoy's jwt_authn extracts the
// same header; the console accepts it so a cookie-only browser session (which
// sends no Authorization header) still presents a verifiable token.
const headerAccessToken = "X-Auth-Request-Access-Token" //nolint:gosec // HTTP header name, not a credential

// bearerToken pulls the credential out of an Authorization header. The
// scheme is case-insensitive per RFC 7235, and proxies do normalize it.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "

	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}

	value := strings.TrimSpace(header[len(scheme):])

	return value, value != ""
}
