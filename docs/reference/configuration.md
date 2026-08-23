# Configuration reference

The service reads **one YAML document**, named by `CONFIG_FILE`, validated at
startup and never reloaded. A worked example is
[`config.example.yaml`](../../config.example.yaml).

Validation is strict on purpose — unknown keys are an error, not a shrug.
This service acts on people's access, and a configuration that is almost
right is the most dangerous kind.

## What lives where

| | Configuration document | Environment |
|---|---|---|
| Contents | organizations, directory sources, teams, intervals | secrets and per-deployment details |
| Safe in git and in a ConfigMap | yes | no |
| Reloaded | never | never |

## Environment

| Variable | Required | Meaning |
|---|---|---|
| `CONFIG_FILE` | yes | path to the configuration document (or `--config`) |
| `LOG_LEVEL`, `LOG_FORMAT` | no | override the document, for a debug session |

That is the whole list, and the shortness is the point — see below.

## Who does the login

**Not this service.** A gateway in front of it runs the OIDC authorization
code flow, holds the session cookie, refreshes the token, validates it
against the issuer's JWKS, and denies anyone outside the console's groups.
The service receives an already-authenticated request with the token
forwarded in `Authorization: Bearer`.

Consequences worth stating, because they are the reason the configuration
looks like this:

- there is **no client secret** here, and none in the environment;
- there is **no session key**, so nothing to share between replicas or
  rotate, and a rollout signs nobody out;
- there are **no sign-in routes** — `/oauth2/callback` and `/logout` belong
  to the gateway.

The service still **verifies the forwarded token itself** against the
issuer's JWKS rather than trusting the header. "We are only reachable through
the gateway" is a property of a NetworkPolicy, one file away from being
wrong, and a service that can remove people's access should not rest its
authorization on that.

The chart's `exposure` values render the gateway side — an `OIDCApp`, an
`HTTPRoute` and a `SecurityPolicy` carrying both the `oidc` block and a
deny-by-default `authorization` rule built from the same `roles` values the
service reads, so the edge and the app cannot disagree about which groups
matter.

## Top level

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:8080` | the console's address |
| `healthListen` | `:7070` | health endpoint, deliberately a separate port so liveness is not routable from outside and does not depend on authentication |
| `logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `logFormat` | `json` | `json` or `text` |

## `oidc`

| Key | Default | Meaning |
|---|---|---|
| `disabled` | `false` | skip token verification entirely, treating everyone as an operator |
| `issuer` | — | the provider's issuer URL; the JWKS is discovered from it at startup |
| `audience` | `""` | when set, required in the token's `aud`; empty accepts any token this issuer minted |
| `rolesClaim` | `groups` | the claim carrying group membership; a string or an array of strings |
| `roles.viewer` | — | claim value granting `viewer` |
| `roles.operator` | — | claim value granting `operator` |

`disabled` must be set explicitly. An empty `issuer` is a configuration
error, never read as "no authentication wanted" — that reading would open an
operator console silently, which is the sort of default that ends up in an
incident report.

`audience` is empty by default rather than required. Behind a gateway that
has already checked it, an audience check here is belt-and-braces, and
access-token audiences differ per provider — Zitadel puts the project there,
not the client — so requiring a value would mostly produce configurations
naming the wrong one.

Someone in both groups is an **operator**. The alternative would make a
person's permissions depend on how their provider happened to sort a claim.

### The two roles

| | `viewer` | `operator` |
|---|---|---|
| Structure page | ✅ | ✅ |
| Audit log | | ✅ |
| Mapping editor | | ✅ |
| Trigger a sync | | ✅ |

There is no third level. Everything an operator can do is either previewed
as a dry run or recorded in the audit trail, and a finer grid would imply a
separation the service does not actually enforce.

## `sources[]`

One corporate directory each.

| Key | Required | Meaning |
|---|---|---|
| `name` | yes | unique; appears in the UI and in audit records |
| `ssmPrefix` | when in-process | holds the directory credentials (service-account key, admin subject); not needed when `endpoint` is set |
| `endpoint` | no | a DirectoryService URL (google-group-sync over ConnectRPC); when set, this source reads through it and holds **no** directory credential |
| `domains` | yes | the email domains this source is responsible for |
| `probeGroup` | no | health canary: a group that always exists (the directory's `all@`, typically) |

`endpoint` moves the Google credential out of this service: instead of a
service-account key under `ssmPrefix`, the source calls a DirectoryService
(google-group-sync) that owns the credential — one shared directory
installation several services can consume. Its snapshot is scoped to the
mapped groups' members (the model only needs people in a team-backing
group), and names come through only when that resolver has the user-read
scope. Set either `ssmPrefix` (in-process reader) or `endpoint` (resolver).

`domains` is required rather than defaulted to "all". A directory may serve
domains this instance has no business managing, and reading them would
import people it should never act on.

`probeGroup` changes what a missing group means. Without it, any error on
any mapped group fails the whole fetch — the safe reading when nothing
proves the directory itself works. With the canary readable, the source
counts as healthy, liveness flows, and a mapped group answering 404 is
recorded as a **per-group absence**: the console warns, and every run
leaves the teams that group backs untouched until it exists. Errors other
than 404 still fail the fetch — auth and transport problems must never
read as absences.

## `orgs[]`

| Key | Required | Meaning |
|---|---|---|
| `name` | yes | the GitHub organization login |
| `consoleAppSSM` | yes | read-only App credentials, held by the web tier |
| `applierAppSSM` | yes | write App credentials, mounted only into reconciler Jobs |
| `exceptions` | no | logins never touched in either direction — Apps, bots |
| `teams` | no | teams whose membership is reconciled |

`consoleAppSSM` and `applierAppSSM` must differ. Pointing both at one
credential dissolves the boundary the whole design rests on, so it is
rejected at startup rather than discovered during an incident.

Exception matching is case-insensitive, because GitHub logins are.

### `orgs[].teams`

Keyed by team name, which must be lowercase alphanumeric with dashes.

| Key | Meaning |
|---|---|
| `groups` | directory groups whose **union** is the team's membership |
| `members` | explicit member emails, unioned with `groups`. A listed address must still belong to a **live** account in its directory — neither a list nor a group ever resurrects a suspended account, even one belonging to a person live elsewhere |
| `pinned` | membership edited only in the operator UI |

A team is directory-mapped (`groups` and/or `members`) or `pinned`,
never both.

Groups are read **flat**: a nested group is not expanded. A team whose
membership you cannot determine by reading the configuration and one group
listing is not a team anyone can review.

**Creating and deleting teams is not this service's business**, and neither
are organization owners. Both are structure, they change rarely, and they
belong in a reviewed infrastructure commit.

## `people`

Optional. Mapping entries declared in the config document itself, merged
**read-only** over the operator-edited store — the git layer wins by name,
and the console refuses edits/deletes of a git-declared entry (it shows as
managed in git). For an installation that wants the mapping, or part of it,
under reviewed pull requests instead of the UI.

| Key | Meaning |
|---|---|
| `name` | the join key, "First Last" |
| `github` | GitHub login |
| `emails` | directory email anchors |
| `k8s` | namespace abbreviation |
| `class` | `employee` or `bot` |
| `pinned` | pinned `<org>/<team>` memberships |

Entries not named here are unaffected and stay fully operator-editable.
Omit the section entirely and the store is the sole source, as before.

## `mapping`

| Key | Default | Meaning |
|---|---|---|
| `ssmPrefix` | `/roster/` | root of the parameter tree; must start and end with `/` |

Person records live under `<ssmPrefix>people/<slug>/`, one parameter per
field:

```
/roster/people/ada-lovelace/name     "Ada Lovelace"
/roster/people/ada-lovelace/github   "ada"
/roster/people/ada-lovelace/emails   "ada@example.com,ada@partner.example"
/roster/people/ada-lovelace/k8s      "ada"
/roster/people/ada-lovelace/class    "employee"
/roster/people/ada-lovelace/pinned   "example-org/robots"
```

`name` is the unique key — the human label every source shares. `emails`
are the IdP connection: the addresses the directories know the person
under, one per identity (a person spanning companies has several). The
join matches on the emails FIRST and falls back to the name, so a
directory spelling a name differently cannot detach a person from their
liveness. The emails also derive the person's *expected* sources: someone
whose only declared directory has never answered is protected from
unattended removal exactly like someone whose directory went stale.

The `people/` segment is not decoration. A prefix should be either a
container or a record, never both — otherwise a person whose name slugged
to `teams` would sit exactly where a future `/roster/teams/` belongs, and
a recursive read could no longer be trusted to return one kind of thing.

Values are written as **SecureString**. The mapping is personal data —
names, work handles, and who is still employed — so it is encrypted at
rest and reading it requires a grant on the KMS key as well as on the
parameter. Nothing stored here is a credential; the reason is privacy and
the access trail, not secrecy.

## `audit`

| Key | Default | Meaning |
|---|---|---|
| `bucket` | — | object storage bucket for run records |
| `prefix` | `""` | roots every record inside the bucket; must end with `/` when set. Empty for a dedicated bucket; the tenant's `<namespace>/<release>/` in the shared-tier test model, where many installs share one bucket and must not see each other |
| `prefixPerOrg` | `true` | file each record under its organization (below `prefix`) |

## `schedule`

| Key | Default | Meaning |
|---|---|---|
| `removalsInterval` | `1h` | how often unattended, removals-only runs happen; `0s` disables the loop (day-0 gating) |
| `maxRemovalFraction` | `0.5` | refuse a run removing more than this share of an organization; `0` disables the guard |

`removalsInterval` *is* the service's half of the revocation SLA: a leaver
loses organization membership within one interval of being suspended in the
directory. Shortening it shortens the SLA and costs a little API quota.

`maxRemovalFraction` is the guard against a directory returning nonsense
convincingly. It is a circuit breaker, not a policy: when it trips, an
operator looks at why rather than raising the number.

## `reconcile`

The continuous reconcile loop (the 0.17 model — see
[reconciliation.md](../architecture/reconciliation.md)) that supersedes the
removals-only `schedule`.

| Key | Default | Meaning |
|---|---|---|
| `interval` | `15m` | how often each *enabled* organization is reconciled; the loop also runs on demand (an operator edit, or Sync now); `0` uses the default |

Per-organization enablement is `orgs[].reconcileEnabled` (set as
`companies.<code>.github.reconcileEnabled`), **born false** — the day-0 gate:
nothing runs unattended until an operator turns it on after a supervised first
sync. While it is false the loop still computes and shows what it *would* do.
