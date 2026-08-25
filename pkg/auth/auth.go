// Package auth turns a request into a Role.
//
// It deliberately does very little — and since 0.34.0, even less lives here:
// token verification, JWKS discovery and key rotation, the oauth2-proxy
// header fallback, the ID-token-cookie and userinfo display-claim fallbacks
// all moved to the fleet-shared github.com/truvity/gateway-auth module (this
// service was the reference implementation those batteries were extracted
// from). What remains is the part that is genuinely this service's:
// mapping the caller's group claims onto the console's two roles, and the
// viewer-or-out gate.
//
// The forwarded token is verified (by the shared module) rather than
// trusted. Not because the gateway is untrustworthy, but because "we are
// only reachable through the gateway" is a property of a NetworkPolicy —
// one YAML file away from being wrong — and a service that can remove
// people's GitHub access should not rest its authorization on that.
package auth

import (
	"context"
	"log/slog"

	"github.com/gofiber/fiber/v3"

	gatewayauth "github.com/truvity/gateway-auth"

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

// CanOperate reports whether the role may mutate state.
func (r Role) CanOperate() bool { return r == RoleOperator }

// CanView reports whether the role may see the console at all.
func (r Role) CanView() bool { return r == RoleViewer || r == RoleOperator }

// Identity is who the caller is, as far as this service cares.
type Identity struct {
	Subject string
	Name    string
	Email   string
	Role    Role
}

// ContextKey is where the identity is parked on the request context.
type ContextKey struct{}

// FromContext returns the identity attached by the middleware, for handlers
// that see a context.Context rather than a fiber.Ctx (the ConnectRPC
// service, adapted through fiber).
func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(ContextKey{}).(Identity)

	return identity, ok
}

// WithIdentity attaches an identity to a context; tests use it to call
// handlers directly.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, ContextKey{}, identity)
}

// From returns the identity the middleware attached to a fiber request.
func From(c fiber.Ctx) (Identity, bool) {
	identity, ok := c.Locals(ContextKey{}).(Identity)

	return identity, ok
}

// Authenticator is what the server mounts.
type Authenticator interface {
	// Middleware authenticates every request, or rejects it.
	Middleware() fiber.Handler
	// Enabled reports whether authentication is actually on.
	Enabled() bool
}

// disabled treats every request as an operator, for local development. It
// must be configured explicitly (oidc.disabled: true) and logs loudly.
type disabled struct{ logger *slog.Logger }

// NewDisabled returns the development-only authenticator.
func NewDisabled(logger *slog.Logger) Authenticator {
	logger.Warn("authentication is DISABLED: every request is treated as an operator")

	return &disabled{logger: logger}
}

func (d *disabled) Enabled() bool { return false }

func (d *disabled) Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		attach(c, Identity{Subject: "dev", Name: "Development", Role: RoleOperator})

		return c.Next()
	}
}

// idTokenCookie is where the gateway's OIDC filter stores the ID token
// (Envoy Gateway's default cookie naming); the shared verifier reads
// display claims from it before asking userinfo.
const idTokenCookie = "IdToken"

// verifier maps the shared module's verified identity onto this console's
// two roles and enforces the viewer-or-out gate.
type verifier struct {
	inner  gatewayauth.Authenticator
	roles  config.Roles
	logger *slog.Logger
}

// NewVerifier builds an authenticator against the issuer's JWKS. Discovery
// and the first key fetch happen inside the shared module at startup, so an
// unreachable or misconfigured issuer fails the rollout rather than the
// first request.
func NewVerifier(ctx context.Context, logger *slog.Logger, cfg config.OIDC) (Authenticator, error) {
	inner, err := gatewayauth.NewVerifier(ctx, gatewayauth.Config{
		Issuer:           cfg.Issuer,
		Audience:         cfg.Audience,
		Claims:           gatewayauth.ClaimsMapper{RolesClaim: cfg.RolesClaim},
		UserinfoFallback: true,
		DisplayCookie:    idTokenCookie,
		Logger:           logger,
	})
	if err != nil {
		return nil, err
	}

	return &verifier{inner: inner, roles: cfg.Roles, logger: logger}, nil
}

func (v *verifier) Enabled() bool { return true }

func (v *verifier) Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		verified, err := v.inner.Authenticate(c.Context(), fiberHeaders{c})
		if err != nil {
			// The reason went to the shared module's log; the caller learns
			// only that it failed — telling an unauthenticated client why
			// its token was rejected helps it produce a better one.
			return fiber.NewError(fiber.StatusUnauthorized,
				"authentication required: this service is reached through the gateway")
		}

		identity := Identity{
			Subject: verified.Subject,
			Name:    verified.Name,
			Email:   verified.Email,
			Role:    roleFor(verified.Roles, v.roles.Viewer, v.roles.Operator),
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

		attach(c, identity)

		return c.Next()
	}
}

// attach parks the identity where BOTH kinds of handler find it: fiber
// Locals for fiber handlers, and the fasthttp request context's user values
// for net/http handlers adapted into fiber (the ConnectRPC service reads it
// with FromContext) — fiber Locals do not cross that boundary on their own.
func attach(c fiber.Ctx, identity Identity) {
	c.Locals(ContextKey{}, identity)
	c.RequestCtx().SetUserValue(ContextKey{}, identity)
}

// fiberHeaders adapts a fiber request to the shared module's Headers view.
type fiberHeaders struct{ c fiber.Ctx }

func (f fiberHeaders) Get(name string) string { return f.c.Get(name) }

// ForwardToken renders the caller's credential as an Authorization value for
// a downstream call made on their behalf (the console forwarding the
// operator's token to the broker). Delegates to the shared module.
func ForwardToken(get func(string) string) string {
	return gatewayauth.ForwardAuthorization(gatewayauth.HeaderGetter(get))
}
