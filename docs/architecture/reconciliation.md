# Continuous reconciliation

**Status:** proposed 2026-08-23. Supersedes the plan/apply flow in
[overview.md](overview.md) (§"The two asymmetries") and the plan-hash API in
[broker.md](broker.md); those documents describe what runs today and will be
rewritten when this ships. This document describes the target the 0.17 line
builds toward.

## The one change

Today an operator approves a **plan**: a diff, content-hashed, applied only
if GitHub still matches the hash. Every addition and every team change needs
a fresh human click, and a configuration change sits unapplied until someone
presses Sync.

Two sources already carry every decision the service needs. The **directory**
says who is employed (live / suspended) and which team groups they are in —
that is HR's decision. The **mapping** says which GitHub account belongs to
which person — that is the operator's decision, the one fact the directory
cannot supply. This design removes the third, redundant decision (approving a
plan) and lets a loop keep GitHub equal to what those two sources say:

```
desired(person, org) = a mapping entry exists
                     ∧ the person is live in the account that governs that org   (home-company rule)
                     ∧ teams = directory groups ∩ configured teams               (derived)
```

**Adding a mapping entry is the approval. Suspension is the removal. Team
membership follows groups and configuration.** Nothing else is approved,
ever. The loop can only add someone who has an entry, and can only remove
someone the directory reports gone — the same two asymmetries the current
"removals-only document" encodes, now as properties of the data rather than a
flag on a run.

## Person state — computed, not stored

A person's state is read off the sources on every tick; nothing stores
"SYNCED". One transition is a human act — **Add**, which writes the mapping
entry. Everything else is the directory or the loop.

| State | Meaning | Waits on | Inline action |
|---|---|---|---|
| `NEW` | Live in a directory and in a team-backing group, but no mapping entry. (Today's `unmapped` warning, promoted to a row.) | operator | Add |
| `PENDING` | Entry exists; GitHub does not match yet. Also where a synced person lands after a group or config change. | loop | Edit |
| `INVITED` | Organization invitation pending — never re-invited, never counted as absent. | the person | Edit |
| `SYNCED` | Every organization and team matches desired. | — | Edit · Remove |
| `LEAVING` | Suspended in an account that governs an org; still a member there. | loop | — |
| `LEFT` | Removed from every org the suspension governs; the entry stays (history, and re-add on reactivation). | directory | Remove (tidy-up) |
| `UNKNOWN` | In GitHub with no mapping entry (added by hand in the GitHub UI). Shown, never auto-removed. | operator | Add · Remove |

Any state may additionally carry a **waiting reason** from a guard —
`source stale`, `no seat`, `too many removals`, `team missing` — shown next
to the badge. It is not a state: the loop retries every tick and it clears
when the cause clears.

`Remove` is the one operator-driven removal: it takes the person out of every
organization and deletes the entry, after a confirm step, and it is what
`Remove` on an `UNKNOWN` row does too. (Today's "delete the mapping, it does
not touch GitHub" is gone: under a loop that would only turn a member into
`UNKNOWN`.)

A returning employee is re-added automatically, because the entry is the
decision and it survives `LEFT`. `BOT`-class entries (pinned teams, no
liveness) sit outside the machine and change only in the operator UI.

## The loop and its guards

Every interval (a setting, default 15 minutes) per enabled organization — and
immediately after an Add/Edit/Remove, a configuration change, or *Sync now*:

```
read      directory snapshots · configuration · GitHub members, invitations, teams
  │
join      → state per person
  │
desired   → per organization and team
  │
diff      → actions: invite · remove · cancel-invite · set-role · team-add · team-remove
  │
guards    → each tripped action WAITS (with a named reason); the rest proceeds
  │
execute   → one audit record per action
  │
status    → last run · next run · waiting actions with reasons
```

Guards are the guards the write path already has; only their consequence
changes — from "refuse the whole plan" to "that one action waits and is
retried".

| Guard | Stops | Because | Clears when |
|---|---|---|---|
| Stale source | removals of people that directory vouches for | A directory that did not answer must never read as "everyone left"; adds and team moves continue from the last good snapshot. | the source reads healthy again |
| Too many removals (shrink breaker) | all removals in that org this tick | Mass suspension in the IdP looks identical to an IdP outage or a bad config. | an operator removes people explicitly (Remove bypasses it), or the count drops under the threshold |
| Minimum owners | any demotion or removal below `minAdmins` | The loop must never lock the organization. | owners change by reviewed configuration |
| Team missing | team actions for a configured team absent on GitHub | Team creation is not this service's business. | the team is created |
| No seat | that one invitation | GitHub refused (422). | seats are bought — retried automatically |
| Pinned team | any change to a pinned team's members | Those are operator-edited by definition. | — |

No "release" button is needed: each waiting action names its cause, and
fixing the cause is the release. The one deliberate human override is
per-person `Remove`, which bypasses the shrink breaker because a human
clicked a name.

The per-organization `enabled` switch is the only on/off control. It is
configuration — a reviewed change — because an unattended write path must be
born disabled and enabled after a supervised first run (the day-0 rule from
the operations postmortems). While disabled, the loop still computes and shows
what it *would* do, which is how the first run is supervised.

## Components and contracts

The service keeps what is its own — configuration, the model, the loop, the
GitHub write — and consumes the rest over ConnectRPC. No new repositories are
required to start; the audit trail is a package here whose contract is written
so it can be lifted out later (e.g. for a second consumer).

- **This service** serves `RosterService` (people with states, teams, status,
  sync, history) and `ConfigService` (directories, organizations and their
  GitHub Apps, teams, people, settings). The console is a credential-less UI
  over these; the broker is the only writer.
- **Directory service** (`DirectoryService`) — an external component that
  answers liveness and flat group membership. The service holds no directory
  credential. A second backend (e.g. a different IdP) is another
  implementation behind the same contract.
- **Audit** — an in-process package with a generic core (who, when, from
  where, caused by what) and a service-typed wrapper, behind an
  `AuditService` contract, with interchangeable record and index stores.

### Configuration — two plugins, one IaC overlay

Configuration lives behind `ConfigService` and is stored through one of two
plugins, selected at deploy time:

- **`ssm`** — AWS SSM Parameter Store under one prefix, one parameter per
  object; secrets as SecureString; parameter versions are the history.
- **`kubernetes`** — one ConfigMap per object kind and one Secret per
  credential, for a cluster with no cloud dependency.

On top of the plugin, an optional **IaC layer** — a YAML document delivered as
a ConfigMap — is merged on every tick, read-only, winning on conflict. This
lets an installation keep structure (directories, organizations, team
bindings, bots) under reviewed pull requests while operators manage people in
the UI, or put everything in git, or run with no git at all. Nothing flows
between the layers: removing an object from the file removes it from the
effective configuration, so there is nothing to prune and nothing that can
drift. Any UI-managed object can be exported as the YAML to move it into git.

```
/roster/settings                 interval, thresholds, defaults
/roster/directories/<abbr>/      endpoint (DirectoryService URL), domains, probe group, enabled
/roster/orgs/<org>/              directory, minAdmins, enabled
/roster/orgs/<org>/app           (secret) app id, installation id, private key
/roster/orgs/<org>/teams/<team>  groups | members | pinned
/roster/people/<name>/           login, emails, k8s, class, pinned teams
```

Organization owners are not stored here — they are read from GitHub and never
computed, the one piece of structure that stays a change made on GitHub
itself.

### Audit — generic core, typed wrapper, two stores

The trail splits a **full record** (actor, client address, cause, the diff,
source health at the time) from a small **index entry** (generic metadata
only). The full record goes to a record store and is never rewritten; the
index answers the History view's filters and paging and is rebuildable from
the records, so it needs no durability of its own.

```
Record { id (ULID) · at · source · scope · kind · subject · actor · client · summary · cause_id · body }
AuditService { Append · List (filters, cursor) · Get · Reindex }
```

Every write record carries `cause_id`, a pointer to the record that authorized
it — an operator's Add, or an observed suspension. That is the "authorized by
an identified person" evidence, readable without opening object storage.

Two store profiles: records in S3 with an index in DynamoDB (AWS), or records
as files on a volume with a SQLite index (Kubernetes). Retention is one
setting (keep-forever by default).

## Authentication

OIDC belongs to the gateway and the deployment, not to this service. What
reaches the service is a bearer JWT, and **every tier verifies it itself**:
the console to decide what to render, the broker on every call to decide what
to accept. Nothing trusts a header or its upstream. Viewer and operator group
names are deployment settings. Nothing about authentication is editable from
the UI.

## The console surfaces

- **People** (home) — the roster as a table with per-organization state
  badges and waiting reasons; presets (*Needs me*, *Active*, *Left*, *Bots*);
  rows expand for the identity trace, an editable form with a live GitHub
  account lookup, and this person's history. *Add* is a slide-over, not a
  page.
- **Teams** — organization → team → backing groups → current vs desired
  members, differences highlighted; editable in place, with the membership
  diff shown on the confirm step.
- **History** — the audit index with filters and a record drawer.
- **Status** — directory health, per-organization last/next run and waiting
  actions, and *Sync now* with the run transcript streaming in.
- **Settings** — directories, organizations (created from the UI via GitHub's
  App manifest flow), teams, and general settings.

The plan-review, run, and per-run audit pages of the current console are gone;
the operator's model becomes "look at *Needs me*; add or remove; the loop does
the rest."

## Failure semantics (unchanged in spirit)

- A failed source fetch suppresses that source's removals; last-known-good is
  kept and staleness is shown.
- A live person with no entry gets nothing and waits for an operator; an entry
  for someone no directory knows is inert.
- The shrink breaker stops a run that would remove an implausible fraction of
  an organization.
- Audit records are one document per change in object storage, never process
  stdout.

## What is unchanged from today

Stateless service; the "First Last" join key with email anchors; the
home-company rule for dual-identity people; directory-mapped vs pinned teams
and the team-naming invariant; team creation and organization owners are not
this service's business; a single-replica broker holding the write
credential; gateway-owned authentication with viewer/operator from claims; and
the mapping's layout in SSM.
