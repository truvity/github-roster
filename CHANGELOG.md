# Changelog

Reconstructed from release tags; one line per release. Full detail lives
in the release notes and the git history.

## 0.15.x
- **0.15.7** — the live-only gate reaches the remaining no-team
  surfaces: the Sync-page banner and the save flash no longer name
  suspended people (0.15.6 fixed only the People-page badge).
- **0.15.6** — the People page's "no team" badge alarms for LIVE people
  only; a suspended person shows a dash — teamless by design, same rule
  the join's warning already followed.
- **0.15.5** — "no team — will never sync" surfaced everywhere it
  matters (#61): Teams column on People, warning flash on mapping save,
  banner on Sync, `no-team` roster warning + `noTeam` on Person. Live
  people only — suspended and orphaned entries keep their own signals.
- **0.15.4** — mapping form prefill works again: the directory-pick
  script moved to `/static/` (the strict CSP was silently blocking it
  inline since 0.15.0) and listens on `input` too (Safari datalist);
  the offered abbreviation now follows the fleet's `emp-{slug5}`
  convention (`otsar`, not `o-tsarev`). Guard test rejects inline
  `<script>` in templates.
- **0.15.3** — friendly 403 page: the gateway's deny answers with a
  configurable HTML body (`exposure.deniedResponseHTML`) instead of
  Envoy's bare "RBAC: access denied".
- **0.15.2** — `emp-` namespace labels; docs overhaul (changelog,
  contributing, operations runbook). Supersedes the retracted v0.15.1.
- **0.15.0** — audit actor identity (subject + email + name) on every
  record; strict CSP with no inline scripts.

## 0.14.x
- **0.14.0** — live run streaming: asynchronous applies narrate over
  Server-Sent Events (replay-then-tail; a stalled watcher never stalls
  the apply).

## 0.13.x
- **0.13.1** — audit *Who* column rides with the actor identity.
- **0.13.0** — audit actor identity: operator-triggered records carry who
  acted, not just "operator".

## 0.12.x
- **0.12.0** — compact person status badges on the People list.

## 0.11.x
- **0.11.1** — absent-group fail-safe no longer swallows a team's
  explicit `members:` — suppressed removals never suppress additions.
- **0.11.0** — in-page run progress; hardening headers.

## 0.10.x
- **0.10.0** — explore surface: one table over the whole join, with
  filters and presets.

## 0.9.x
- **0.9.0** — information-architecture rework: one question per page;
  person detail page.

## 0.8.x
- **0.8.0** — scoped organization reads; run history on the sync page.

## 0.7.x
- **0.7.0** — UI tabs: Overview, Structure, People.

## 0.6.x
- **0.6.0** — the broker owns the unattended removals path; the Job path
  is removed entirely.

## 0.5.x
- **0.5.0** — explicit team `members:` lists (union with groups),
  per-organization `minAdmins`, 60s route timeout.

## 0.4.x
- **0.4.1** — broker: disjoint pod selectors, 64KiB header buffer (fixes
  431s when operator cookies grew past fasthttp's default).
- **0.4.0** — **applier broker**: intent-only API, content-hash-confirmed
  applies; the write credential leaves the Job model.

## 0.3.x
- **0.3.1** — pinned teams keep their current members.
- **0.3.0** — native applier replaces upstream peribolos in the Job.

## 0.2.x
- **0.2.10** — stateless same-origin CSRF guard.
- **0.2.9** — spread constraints, insurance health exemption,
  zero-interval gate.
- **0.2.8** — probe-group health canary, expected-source fail-safe,
  declared emails, form prefill.
- **0.2.7** — emails as the IdP anchor, identity trace columns, session
  affinity.
- **0.2.6 and earlier** — skeleton, read layers, the join, the mapping
  editor, reconciler orchestration, audit, schedule and guardrails (the
  original seven build phases).
