# The applier broker

Status: accepted 2026-08-01. Replaces the per-run reconciler Job as the
holder of the write credential.

## Why a service instead of a Job

The Job model (v0.3.x) sealed the write credential away from the web tier,
but it had two structural weaknesses and one practical one:

1. **The console supplied content.** The console rendered the membership
   document and handed it to the Job. A fully compromised console could
   render a malicious document and create a confirmed Job carrying it —
   the guards bound the blast radius, they do not remove the path.
2. **Approval was procedural, not cryptographic.** The Job trusted that
   whoever created it had seen a preview. Nothing bound the executed plan
   to a plan a human actually reviewed.
3. Every preview paid ~30s of pod startup and full-organization reads,
   which pushed the synchronous UI past every timeout in front of it.

The broker fixes all three by inverting the data flow: **the console sends
intent, never content.**

## The model

One long-lived internal service — the broker — holds the applier App
credentials, an in-memory cache of organization state, and an in-memory
store of plans it has computed.

```
browser ── gateway (OIDC) ── console ── broker ── GitHub (write App)
                                          │
                                          ├── directory (Google)
                                          ├── SSM mapping
                                          └── S3 audit
```

The API is deliberately thin — what is absent from it is a write the
console cannot cause:

- `POST /v1/orgs/{org}/plans` — compute a plan. The broker renders desired
  state ITSELF from the directory and the SSM mapping, reads live GitHub
  state, diffs, and stores the plan keyed by its content hash. Returns the
  plan and its hash. This is the dry run: instant when the state cache is
  warm.
- `GET /v1/orgs/{org}/plans/{hash}` — re-read a stored plan.
- `POST /v1/orgs/{org}/plans/{hash}/apply` — re-read live state, recompute,
  and execute ONLY if the recomputed plan's hash equals `{hash}`. Any
  drift → 409 with the fresh plan, and the operator reviews again. The
  operator therefore approves exactly what executes, never approximately.
- `GET /healthz`.

## Security properties

- **Intent-only interface.** The console cannot supply a document, a member
  list, or a plan body — only "plan a sync" and "apply the plan with this
  hash". A compromised console cannot forge what gets applied.
- **Operator JWT re-verified at the broker.** Plan and apply requests carry
  the end-user's gateway JWT; the broker verifies it against the same
  issuer/JWKS and requires the operator role claim. A compromised web pod
  without a live operator session cannot execute an apply.
- **Guards live with the credential.** min-admins, the removal circuit
  breaker, removals-only-computes-no-adds, unknown-team refusal, and
  pinned-team preservation are enforced inside the broker.
- **Audit is unskippable.** The broker writes the S3 record for every plan
  and every apply itself.
- **Network policy.** Ingress: console only. Egress: GitHub, the directory,
  SSM, S3.
- The one accepted trade: the write key resides in a long-lived process
  rather than 30-second Jobs. The thin API, JWT gate, netpol, and
  single-purpose deployment are the compensation, and they net out safer
  than "any process that can create Jobs in the namespace can drive a
  write".

## State and caching

Organization state is cached in-memory with a short TTL: plans compute
instantly from warm state; an apply always re-reads live before the drift
check, scoped to the organization's members, invitations, and the teams
the document names. Plans expire after an hour — an operator who walks
away re-reviews. All of it fits in memory many times over; an external
cache (valkey/redis) was considered and rejected: it would put membership
decisions on a network hop and add a consistency surface to the most
sensitive component we run, for data measured in tens of kilobytes.

Single replica. Applies take seconds; the run lock is a mutex, not a
distributed system.

## The unattended path

The removals-only ticker and the insurance CronJob (both currently gated
off pending day-0) move inside the broker: it computes and applies its own
removals-only plans on schedule, no JWT involved, gated by the same
configuration switch as today. The rendered-document invariants
(removals-only can never add; unreadable sources suppress removals) are
unchanged.

## What the console keeps

Reading, joining, and showing: directory health, the roster join, mapping
editing, and the sync pages — which now render the broker's plans instead
of spawning Jobs. The console keeps its read-only GitHub App for the
Structure pages; the broker does its own reading with its own credential
and never trusts console-supplied state.
