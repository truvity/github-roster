# Google Workspace "Connect" onboarding (design)

Status: **designed, not yet implemented** — blocked on one human
prerequisite (the OAuth client below). This documents the target flow and
what exists already, so the implementation session starts from decisions,
not archaeology.

## Goal

Tailscale-grade onboarding for a directory: an operator clicks **Connect
Google Workspace**, authenticates as a Workspace admin, confirms the
consent screen, and the roster is reading users and groups — no service
account JSON handed around, no domain-wide-delegation checklist in a
wiki.

Today's onboarding is the manual inverse: create a GCP service account,
enable domain-wide delegation, paste the key into SSM under
`/secrets/google-workspace/<code>`, grant the Admin SDK scopes in the
Workspace admin console. It works (both current companies run on it) but
every step is a support ticket.

## The precedent to mirror

The GitHub side already has exactly this shape — the **App-manifest
flow**: Settings stages an org, shows "Create GitHub App →", the server
(`handleCreateApp`) hands GitHub a self-submitting manifest form, GitHub
redirects back to `/settings/orgs/app-callback` (CSRF-guarded by a state
cookie), and the credentials land in the org store
(`OrgStore.PutApp`). The Google flow is the same dance with Google's
pieces.

## Flow

1. **Prerequisite (human, once per installation):** register an OAuth
   client in GCP — type *Web application*, redirect URI
   `https://<console>/settings/directories/google-callback`. Store its
   `client_id`/`client_secret` in SSM under
   `/secrets/roster/google-oauth/{client-id,client-secret}` (mirrored by
   the same mechanism as the other console credentials). The client can
   live in any GCP project; it does NOT need to belong to the Workspace
   being connected.
2. Settings → Directories → **Connect Google Workspace**: operator
   enters the directory name + domains (per-domain form, as 0.30.0).
3. Server (`handleConnectGoogle`, operator-gated) redirects to Google's
   consent URL: scopes
   `admin.directory.user.readonly admin.directory.group.readonly
   admin.directory.group.member.readonly`, `access_type=offline`,
   `prompt=consent` (forces a refresh token), plus a `state` cookie
   (same CSRF pattern as the App callback).
4. The person authenticating must be a Workspace admin (the Admin SDK
   authorizes per-user; a non-admin gets 403s on the first read — the
   probe surfaces that immediately).
5. Callback exchanges the code, stores the **refresh token** under the
   directory's `ssmPrefix` (`google-oauth-refresh-token`), stores the
   admin's email (`google-admin-email`, already a known field), and
   writes the directory into the DirectoryStore — born with every domain
   `sync: false` (display-only) so nothing is granted until the operator
   reviews and flips the switch. Day-0 gate, same philosophy as staged
   orgs.
6. First fetch runs the per-domain probes; Settings shows per-domain
   health. Operator verifies, then enables sync per domain.

## Code changes (when unblocked)

- `pkg/directory/google.go`: `GoogleConfig` grows an alternative
  credential — `RefreshToken` + `OAuthClient` — beside `KeyJSON`;
  `service()` builds the client from `oauth2.Config.TokenSource` instead
  of JWT domain-wide delegation when the refresh token is set. Reads are
  identical after auth.
- `pkg/secrets`: read the two new fields.
- `pkg/server`: `handleConnectGoogle` + `handleGoogleCallback`
  (mirroring `app_manifest.go`), an RPC to start the flow from the SPA.
- Settings UI: the Connect button + per-domain enable after review.
- Rotation: a revoked refresh token surfaces as the source unhealthy
  (probe fails); reconnecting is the same button again.

## Why OAuth-refresh rather than a service account

The admin-consent flow removes the two failure-prone human steps
(key handling, delegation setup), and revocation is visible in the
Workspace admin console. The trade: reads run as a *person* (token dies
with their account), so the doc must say clearly — connect as a
role/service admin account, not a personal one. The service-account path
stays supported for installations that prefer robot identity; `Connect`
is the onboarding fast path, not a replacement.
