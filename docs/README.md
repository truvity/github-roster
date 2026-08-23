# github-roster documentation

| Document | What it covers |
|---|---|
| [architecture/overview.md](architecture/overview.md) | What the service is, the three sources it joins, the two asymmetries, and the write boundary |
| [architecture/broker.md](architecture/broker.md) | The applier broker: intent-only API, hash-confirmed applies, security properties |
| [architecture/reconciliation.md](architecture/reconciliation.md) | **Proposed:** one decision (the mapping entry) and a continuous reconcile loop; ConnectRPC config/directory/audit; the target for the 0.17 line |
| [operations/runbook.md](operations/runbook.md) | On-call knowledge: restart semantics, failure signatures, recovery |
| [reference/configuration.md](reference/configuration.md) | Every configuration key, and what belongs in the environment instead |
| [reference/roster-api.md](reference/roster-api.md) | `GET /api/roster` — the cross-repository contract, and how to read it safely |
| [development/testing.md](development/testing.md) | Unit, integration and end-to-end test layers, and what each needs |

Repo-level: [CHANGELOG.md](../CHANGELOG.md) ·
[CONTRIBUTING.md](../CONTRIBUTING.md) · [SECURITY.md](../SECURITY.md).

## How the service grew

The original seven build phases (skeleton → read layers → join → mapping
editor → reconciler orchestration → audit → schedule and guardrails)
shipped as 0.1–0.2. Since then the write path moved twice — peribolos
Jobs → a native applier Job (0.3) → the applier broker (0.4, removals
path 0.6) — and the console grew its explore/person/audit surfaces
(0.7–0.12), actor identity on audit records (0.13), live SSE run
streaming (0.14), and a strict no-inline-scripts CSP (0.15). One line
per release in [CHANGELOG.md](../CHANGELOG.md).
