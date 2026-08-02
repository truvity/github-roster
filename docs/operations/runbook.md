# Operations runbook

What a person on call needs to know. Everything here is generic to any
deployment of the chart; organization-specific values (hostnames, bucket
names, group names) belong in your own infrastructure notes.

## Shape

Two deployments from one image:

- **console** — read-only GitHub App, renders every page, edits the
  mapping. Stateless; scale as you like.
- **broker** — the only holder of the write-capable App credential.
  Deliberately single-replica with the `Recreate` strategy: plans, run
  transcripts and the state cache are in-memory, derived data, and an
  apply serializes on one mutex.

Health endpoints are on a separate, unauthenticated port (`healthListen`)
that should not be routable from outside the cluster.

## Restart semantics

A broker restart loses unapplied plans and live run transcripts — never
anything durable. The operator recomputes a plan in seconds; the audit
record in object storage remains the account of anything that actually
ran. Restarting the broker is therefore always safe, and it is the first
move when its state cache is suspected of being wrong.

## What the UI tells you

- **Overview** — per-source directory health with last-known-good ages. A
  stale source means fetches are failing; its removals are suppressed
  (fail-safe) until it recovers, and nothing else stops working.
- **Sync** — plans and run history. An asynchronous apply streams its
  transcript live (SSE); a finished run's transcript stays readable for
  an hour, then only the audit record remains.
- **Audit** — every plan and apply, with the actor's identity on
  operator-triggered records.

## Failure signatures

| Symptom | Likely cause | Move |
|---|---|---|
| Operator pages return **431** | forwarded auth cookies exceed the server header buffer | the chart sets a 64KiB buffer on both deployments; check an ingress in front is not clamping lower |
| Apply returns **409** with a fresh plan | live GitHub state changed since the plan was reviewed | expected behavior — review the fresh plan and apply again |
| Apply fails with **422 no_seat** | the organization is out of paid seats | buy seats, retry the same plan (the hash still matches if nothing else moved) |
| A team's plan keeps proposing the same additions | invitees have not accepted; a pending invitation is neither re-invited nor counted as absent | wait, or chase the invitee |
| A directory source shows stale on Overview | credential expiry, API quota, network egress | check broker/console logs for the fetch error; removals stay suppressed meanwhile |
| Run page says "no such run" | transcript expired (1h) or the broker restarted | read the audit record instead |
| A plan would remove many people at once | shrink threshold trips the circuit breaker | verify the directory is healthy before overriding anything — mass suspension in the IdP looks identical to an IdP outage |

## The mapping is recoverable

The mapping lives in SSM Parameter Store, one parameter set per person,
and parameter **versions are its history**. A bad edit is undone by
reading the previous version and putting it back — there is no database
to restore.

## The unattended path

The removals-only ticker and the insurance CronJob are configuration-gated.
When enabled, the broker computes and applies removals-only plans on
schedule: nobody not already a member can appear in such a plan, so the
only possible change is a removal. If sources are unhealthy the run
suppresses removals rather than guessing.
