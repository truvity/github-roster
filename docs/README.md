# github-roster documentation

| Document | What it covers |
|---|---|
| [architecture/overview.md](architecture/overview.md) | What the service is, the three sources it joins, and why the write path is a separate Job |
| [reference/configuration.md](reference/configuration.md) | Every configuration key, and what belongs in the environment instead |
| [reference/roster-api.md](reference/roster-api.md) | `GET /api/roster` — the cross-repository contract, and how to read it safely |
| [development/testing.md](development/testing.md) | Unit, integration and end-to-end test layers, and what each needs |

Arriving with the phase that makes it true: `operations/runbook.md`
(phase 5).

## Build order

The service is built in phases, one pull request each, each with green CI:

1. **Skeleton** — configuration, health endpoint, server-rendered layout,
   OIDC middleware with `viewer`/`operator` roles, structured logging.
2. **Read layers** — directory sources (liveness + groups) with per-source
   last-known-good, the SSM mapping reader, GitHub read state including
   pending invitations.
3. **Join** — liveness ⋈ mapping ⋈ GitHub, and `GET /roster`.
4. **Mapping editor** — operator-only forms, server-enforced invariants,
   bulk import.
5. **Reconciler orchestration** — rendering the membership document,
   calling the applier broker: instant preview, hash-confirmed apply.
6. **Audit** — one record per run to S3, and `GET /audit`.
7. **Schedule and guardrails** — the removals-only ticker, staleness
   handling, shrink threshold, insurance CronJob.
