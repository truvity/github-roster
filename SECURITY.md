# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via [GitHub Security Advisories](https://github.com/truvity/github-roster/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

| Version | Supported |
|---------|-----------|
| latest  | ✅        |
| older   | ❌        |

## Design notes relevant to a reviewer

- The web tier holds a **read-only** GitHub App credential. Every write to
  GitHub happens in the **applier broker** — a separate deployment that
  alone holds a *different*, write-capable App's credential behind an
  intent-only API: the console sends verbs, never content, and an apply
  executes only when a fresh recomputation still matches the content hash
  the operator approved. Compromising the web tier does not grant the
  ability to mutate an organization — nor to forge what would change.
- The broker re-verifies the operator's JWT against the issuer's JWKS on
  every plan and apply; every run writes an audit record with the actor's
  identity.
- Scheduled, unattended runs can only ever **remove** organization members.
  Additions require a signed-in operator. This asymmetry is a property of
  the rendered membership document, not a command-line flag.
- The service holds no git credentials and no database.
