# `GET /api/roster`

The joined view: directory liveness ⋈ the mapping ⋈ GitHub state.

**This is a cross-repository contract.** A puller commits the response so a
configuration generator can build developer namespaces from git alone.
Fields may be added; removing or repurposing one breaks a consumer that
does not live in this repository. The machine-readable description is
served at `/api/openapi.json` and generated from the response type, so it
cannot drift from the implementation.

## Shape

```json
{
  "generatedAt": "2026-07-29T13:24:10Z",
  "sources": [
    { "source": "corp", "healthy": true, "ready": true,
      "fetchedAt": "2026-07-29T13:20:00Z", "age": 250000000000 }
  ],
  "people": [
    {
      "name": "Ada Lovelace",
      "github": "ada",
      "k8s": "ada",
      "class": "employee",
      "live": true,
      "sources": ["corp"],
      "email": "ada@example.com",
      "orgs": {
        "example-org": {
          "member": true,
          "role": "member",
          "teams": ["engineers"],
          "desiredTeams": ["engineers"]
        }
      }
    }
  ],
  "warnings": [
    { "kind": "unmapped", "subject": "New Joiner",
      "detail": "live in corp but has no mapping entry, so nothing is granted" }
  ]
}
```

## What is in `people`, and what is not

`people` holds everyone the join could **identify** — that is, everyone
with a mapping entry. It is deliberately not "everyone in the directory":

- a live person with **no mapping entry** is not a `Person`. They appear as
  an `unmapped` warning and are granted nothing. Inventing a GitHub handle
  from an email address is the alternative, and it is how someone ends up
  with access to the wrong account;
- a mapping entry **no directory knows** is present but inert: `live` is
  false, `sources` is empty, and `desiredTeams` is empty everywhere.

## Fields worth understanding

| Field | Meaning |
|---|---|
| `live` | the person still has an unsuspended account in **at least one** directory. A `bot` is always live — it has no directory account, and reading that as "gone" would delete every bot on the first scheduled run |
| `sources` | which directories know them. Someone can legitimately appear in several |
| `orgs[].live` | liveness **for that organization** (the home-company rule): the identity in the org's own company governs when one exists — suspended there, the person is a leaver for that org alone, whatever their other accounts say. A person with no identity in the org's company (a partner) inherits person-level `live` |
| `orgs[].member` | member **or** holder of a pending invitation — both occupy a seat |
| `orgs[].invitationPending` | distinguishes the two, because they need different actions |
| `orgs[].teams` | what GitHub says today |
| `orgs[].desiredTeams` | what the configuration says. The difference is what an operator sync would change |

`desiredTeams` is empty for anyone not live **for that org**, whatever
their directory groups still say — group membership routinely outlives
suspension. The gate is per account, too: a suspended account's group
memberships and `members:` listings grant nothing even while the person
stays live through another directory.

## Warnings

Warnings never block the response. A roster that refused to render because
one person is unmapped would be useless precisely when it is needed.

| Kind | Meaning | Who fixes it |
|---|---|---|
| `unmapped` | live person with no mapping entry | an operator, in the mapping editor |
| `orphaned-mapping` | mapping entry no directory knows | an operator — usually a leaver whose entry outlived them, or a misspelled name |
| `unknown-member` | GitHub member matching no entry and not an exception | an operator; this is what an adoption cleanup is made of |
| `not-live-owner` | an organization **owner** no longer live for that org — gone everywhere, or suspended in the org's own company | **not a sync.** Owners are registry-pinned; this needs a reviewed infrastructure change |
| `stale-source` | a directory's last fetch failed | nobody, immediately — but this source's removals must be skipped |

## Reading it safely

A consumer that acts on this document should check `sources` first. A
roster built while a directory was unreachable still lists that
directory's people from the last known good read — which is correct for
display, and **not** a basis for removing anyone.

The rule the service applies to itself, and that any consumer should
apply too: **missing information produces inaction, never action.**

## Errors

`500` with `{"error": "could not build the roster"}` when the mapping
store cannot be read. A single unreadable *organization* is not an error —
it is omitted from every person's `orgs`, and the reason is logged. That
asymmetry is deliberate: an absent organization reads as "nothing known",
whereas an empty mapping would read as "nobody works here".
