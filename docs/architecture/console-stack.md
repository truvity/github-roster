# The console stack: how to build an internal operator app

github-roster is the **reference implementation** of the fleet's console
stack. This document is the transferable part: what the stack is, when to
use it, and the traps we hit so the next console (gemaal's panel when it
grows write-actions, the business apps) doesn't hit them again.

## The stack, and what each piece is for

| Layer | Choice | Why |
|---|---|---|
| Backend | **Go** (fiber v3) | one binary, one deploy; no Node backend — there is no Nest.js/Next.js tier anywhere and none is needed |
| API | **ConnectRPC** (connect-go + protobuf-es v2 / Connect-Web) | one proto contract shared by server and SPA; a drifted field is a compile error, not a runtime surprise; plain HTTP/1.1 so Cloudflare tunnels and oauth2-proxy pass it untouched |
| Frontend | **Vite + React + TypeScript + Material UI** | static build, embedded via `go:embed`, served by the same binary — no separate frontend deploy, same-origin so the strict CSP holds |
| AuthN | **gateway-auth** (oauth2-proxy ext_authz + Valkey) in front | the app never runs a login; it receives `Authorization` (validated JWT) + `X-Auth-Request-*` and maps claims to two roles (viewer/operator) |
| Codegen | **buf** with remote plugins; `gen/` is **committed** | CI needs no buf/protoc — regeneration happens only when the contract changes (`just generate`) |

When to use it: any internal app with an authenticated human surface and
a service API. When not to: a read-mostly status panel with no write
actions — html/template with zero build steps (gemaal's panel today) is
cheaper and legitimate; migrate when operators need to *act*, not look.

## Layout (mirror this)

```
proto/<app>/v1/<app>.proto   the one contract
gen/                         committed Go codegen (buf generate)
frontend/src/gen/            committed TS codegen (same buf run)
frontend/                    Vite app; dist/ is committed and embedded
  src/api.ts                 the ONLY file that touches the Connect client;
                             maps proto → view types (empty scalar → undefined)
  src/theme.ts  hooks.ts  ui.tsx   theme, useAsync, shared chips/traces
  src/<View>.tsx             one file per tab
pkg/server/connect.go        ConnectRPC handlers (thin: authz gate → deps call → proto map)
pkg/server/server.go         fiber app: CSP, auth middleware, SPA mount, routes
frontend/embed.go            //go:embed dist
```

## Rules that came from real incidents

1. **Never run long work inside a request.** The gateway's route timeout
   (~15 s) is shorter than you think. Sync-now ran a ~23 s pass
   synchronously: the server answered 200 to a browser that already had
   a 504. Pattern: the RPC *triggers* (detached, bounded context;
   honest 409 if busy), the UI polls a status until a timestamp
   advances. A server-stream is NOT the easy fix — a long-lived stream
   dies by the same route timeout unless the gateway carves it out;
   that's its own project (`WatchStatus`, designed, not built).

2. **Views live in the URL fragment** (`#status`), state in React. Then
   the server needs no catch-all route: `/` serves index, `/assets/*`
   the hashed files, and every API path stays clean. Deep links, back
   button and refresh all work with ~15 lines (a `hashchange` listener).

3. **Derived state must be computed where it's used, and asserted.**
   `membershipState()` existed, was unit-tested, and was never called —
   every person's state was empty and the UI filters silently matched
   nobody. Unit tests on a pure function prove nothing about wiring:
   test at the *join/handler* level (the regression proves FAIL without
   the call, PASS with).

4. **Two data tiers, two authorities.** The snapshot a background loop
   holds (last pass) and the live flags an operator just flipped are
   different truths. Overlay live control flags onto snapshots at the
   read edge (`liveControl`), or every toggle looks broken for one tick.

5. **Writes happen where the write grant lives.** The console holds the
   SSM write role; the broker is deliberately read-only (it holds the
   GitHub applier credential — blast-radius boundary). Routing a
   console write *through* the broker produced AccessDenied. Keep the
   privilege map in your head when adding an RPC: who may write this?

6. **Strict config parsing** (`yaml.KnownFields(true)`) + scalar-or-
   mapping `UnmarshalYAML` shorthands. A renamed field then fails the
   rollout loudly instead of silently dropping a health canary. Ship
   config migrations in the SAME gitops PR as the version pin.

7. **MUI specifics.** Community (MIT) tier only: `Table stickyHeader` +
   `TableSortLabel` + `Collapse` gives sortable/sticky/expandable tables
   without DataGrid Pro (community DataGrid has no detail panels).
   Density: compact defaults in the theme (`size="small"` via
   `components.defaultProps`). Bundle lands ~176 KB gzip — fine for an
   internal tool; don't chase code-splitting until it hurts.

8. **api.ts is the anti-corruption layer.** Views never import
   generated types; `api.ts` maps proto messages to view interfaces and
   normalizes empty scalars to `undefined`. Proto churn then touches
   one file.

9. **Hybrid config ownership everywhere.** Git-declared = reviewed,
   read-only baseline (a PR is the gate); operator-added = UI-editable
   overlay in a store, refused — not hidden — when it would shadow git
   (`FailedPrecondition`). Same split for directories, orgs, teams,
   people.

10. **Release mechanics.** Tag `v*` → chart+image publish (~5 min);
    bump the gitops pin only AFTER the release lands; pin PRs auto-merge
    on green. The golden-update trap and the exact procedure live in
    the gitops repo's operator notes.

## Adopting this for an existing SSR app (the gemaal path, later)

The migration order that worked here, each step shippable alone:
proto + ConnectRPC endpoints beside the existing pages → SPA serving the
read views → port write actions (operator-gated RPCs) → retire SSR →
polish (hash routing, root mount). Total for the roster: ~10 releases,
each independently deployed and reverted-able.
