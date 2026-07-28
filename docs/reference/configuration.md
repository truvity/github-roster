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
| `ssmPrefix` | yes | holds the directory credentials (service-account key, admin subject) |
| `domains` | yes | the email domains this source is responsible for |

`domains` is required rather than defaulted to "all". A directory may serve
domains this instance has no business managing, and reading them would
import people it should never act on.

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
| `pinned` | membership edited only in the operator UI |

A team is one or the other, never both, and never neither.

Groups are read **flat**: a nested group is not expanded. A team whose
membership you cannot determine by reading the configuration and one group
listing is not a team anyone can review.

**Creating and deleting teams is not this service's business**, and neither
are organization owners. Both are structure, they change rarely, and they
belong in a reviewed infrastructure commit.

## `mapping`

| Key | Default | Meaning |
|---|---|---|
| `ssmPrefix` | `/roster/` | root of the parameter tree; must start and end with `/` |

## `audit`

| Key | Default | Meaning |
|---|---|---|
| `bucket` | — | object storage bucket for run records |
| `prefixPerOrg` | `true` | file each record under its organization |

## `schedule`

| Key | Default | Meaning |
|---|---|---|
| `removalsInterval` | `1h` | how often unattended, removals-only runs happen |
| `maxRemovalFraction` | `0.5` | refuse a run removing more than this share of an organization; `0` disables the guard |

`removalsInterval` *is* the service's half of the revocation SLA: a leaver
loses organization membership within one interval of being suspended in the
directory. Shortening it shortens the SLA and costs a little API quota.

`maxRemovalFraction` is the guard against a directory returning nonsense
convincingly. It is a circuit breaker, not a policy: when it trips, an
operator looks at why rather than raising the number.
