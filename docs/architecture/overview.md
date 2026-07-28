# Architecture

## The problem

Below GitHub Enterprise there is no SAML and no SCIM, so an organization on
the Team plan has no automated link between "employed" and "member of the
GitHub org". Joining is a manual invite; leaving is a manual removal that
someone has to remember. The failure mode is asymmetric and well known:
joiners get chased, leavers get forgotten.

Buying Enterprise solves it for roughly five times the per-seat price, and
its native team synchronization supports only some identity providers
anyway. This service closes the gap at the cost of one small stateless
process.

## The three sources

Nothing here is a database. The service joins three things it does not own:

1. **Directory liveness and groups.** Per configured source: who exists,
   who is suspended, and who is in which group. A source is a corporate
   directory; suspension is the authoritative "not live" signal.
2. **The mapping.** "First Last" → GitHub handle, plus a Kubernetes-safe
   abbreviation. Held in AWS SSM Parameter Store, one parameter set per
   person, edited only through this service's operator UI. Parameter
   versions are its history.
3. **GitHub read state.** Members, teams and — critically — *pending
   invitations*, read with a read-only App.

The join key is the person's display name, "First Last". That is a
deliberate choice with a known sharp edge: two people with identical names
collide. Email would be a better key, but a person can exist in more than
one directory with more than one address, and the GitHub handle is the
thing being mapped — so the name is the only identifier common to every
source.

## Directory-mapped and pinned teams

A team is one of two kinds:

- **Directory-mapped**: its membership is the union of the configured
  groups. Group membership is read flat — nested groups are deliberately
  not resolved, because a nested group makes "who is in this team" a
  question you cannot answer by looking. One group edit onboards or
  offboards a person everywhere.
- **Pinned**: its membership is edited only in the operator UI and stored
  with the mapping. Scheduled runs never touch a pinned team. This is where
  people and machine accounts live that have no directory-group answer —
  auditors, bots.

Each mapping entry carries a class: `employee` (governed by directory
liveness) or `bot` (pinned, no liveness signal).

**Team creation and deletion are not this service's business,** and neither
are organization owners. Both are structure, they change rarely, and they
are the sort of change that should arrive as a reviewed infrastructure
commit. The service reconciles *membership* of teams that already exist.

## The two asymmetries

### Removals are automatic; additions are not

An unattended run renders a config from *current GitHub state minus
people known not to be live*. Nobody who is not already a member can appear
in that document, so the only change it can produce is a removal. An
operator-triggered sync renders the full desired state, and the operator
sees a dry-run diff before confirming.

The asymmetry is a property of the rendered document rather than a flag on
the reconciler, because a document can be read, diffed and stored in the
audit record — a flag cannot.

### Scheduled runs touch organization membership only

Removing someone from the organization removes their access through every
team at once, so the revocation SLA needs nothing more. Team membership
changes — in both directions — flow through an operator sync. This keeps
the surface that acts without a human as small as it can be while still
meeting the SLA.

## The write boundary

The web tier holds a **read-only** GitHub App. Writes happen in a
short-lived Kubernetes Job that mounts a **different** App, one with write
permission. The Job runs an unmodified upstream
[peribolos](https://github.com/kubernetes-sigs/prow/tree/main/cmd/peribolos)
image against a config the service rendered into a ConfigMap.

The gain is concrete: an attacker who compromises the web process can read
your organization and lie to your operators, but cannot change a single
membership. The cost is one indirection and a slower feedback loop.

Peribolos was chosen over an in-process API client because the parts of the
problem that are actually hard — invitation lifecycle, membership versus
team membership, pagination, rate limits, exempting bots — are production-
hardened there at a scale we will never reach.

## Where authentication happens

Not here. A gateway in front of the console runs the OIDC authorization code
flow, owns the session cookie, refreshes the token, validates it against the
issuer's key set, and — separately, and just as importantly — denies anyone
whose groups are not on the console's list. Being authenticated is never
sufficient; any account in the organization would otherwise reach an
operator console.

The service therefore holds no client secret, mints no sessions, and has no
sign-in routes. What it keeps is the part a gateway cannot do for it:
deciding whether this caller is a viewer or an operator, because that answer
changes what renders and what the handlers accept.

It still verifies the forwarded token against the issuer's key set rather
than trusting the header. Not because the gateway is untrustworthy, but
because "only reachable through the gateway" is a property of a network
policy — one file away from being wrong — and a service that can remove
people's access should not rest its authorization on that.

## Failure semantics

- **A failed source fetch skips that source's removals.** Missing data must
  never be read as "everyone left". Each source keeps a last-known-good
  value, and staleness is shown in the UI rather than hidden.
- **Fail-safe joins.** A live person with no mapping entry gets nothing and
  waits for an operator. A mapping entry for someone no directory knows is
  inert.
- **A shrink threshold** stops a run that would remove an implausible
  fraction of the organization.
- **Audit records go to object storage, one JSON document per run**, never
  to the process's stdout. A log line that scrolls away is not an audit
  trail.
