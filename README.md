# github-roster

**GitHub membership console for organizations without Enterprise** —
directory-joined liveness, an audit trail, and one-click or scheduled sync.

Below GitHub Enterprise there is no SAML and no SCIM. An organization on the
Team plan has no way to say "this person left the company, revoke their
access" other than a human remembering to click *Remove from organization*.
`github-roster` closes that gap without the Enterprise upgrade: it joins your
identity provider's liveness signal against a small, explicit mapping of
people to GitHub handles, and reconciles organization membership from it.

- **Stateless.** No database. State lives in your directory, in AWS SSM
  Parameter Store (the mapping), in S3 (the audit trail), and in GitHub
  itself.
- **Server-rendered.** Plain HTML, no SPA, no build step for the front end.
- **Two credentials, one boundary.** The web tier holds a read-only GitHub
  App. Every write runs in a short-lived Kubernetes Job holding a
  *different*, write-capable App. Compromising the console does not grant
  the ability to mutate your organization.
- **Removals are automatic; additions are not.** Unattended runs render a
  reconciler config from which only removals are possible. Adding people
  requires a signed-in operator who has seen a dry-run diff.

## How it works

```
directory (liveness + groups)  ─┐
SSM mapping (name → handle)    ─┼─►  join  ──►  desired state  ──►  applier Job  ──►  GitHub
GitHub read state              ─┘                    │
                                                     └──►  audit record (S3)
```

The reconciler is the service's own `apply` subcommand, run as a
short-lived Job from the same image as the console but holding a different,
write-capable credential. Its scope is a type-level property: organization
membership (invitations included — a pending invite is neither re-invited
nor miscounted) and membership of the teams named in the rendered document.
There is no code in it that can create, delete, or edit a team, or touch
organization settings.

Everything the service decides is a matter of *rendering the membership
document*. The removals-only asymmetry of unattended runs is a property of
that rendered document, re-checked by the subcommand — which is what makes
it reviewable.

## Status

Under construction. See [docs/](docs/) for the design and the phase plan.

## Quickstart

```bash
devbox shell          # or: install Go, just, helm, golangci-lint
just check            # build + unit tests + lint + chart render
just build && ./bin/github-roster version
```

Configuration is a single YAML document; see
[docs/reference/configuration.md](docs/reference/configuration.md).

## Deployment

A Helm chart lives in [charts/github-roster](charts/github-roster). Each
tagged release publishes an image to
`ghcr.io/truvity/github-roster` and the chart to
`oci://ghcr.io/truvity/charts/github-roster`.

## Contributing

Bug reports and pull requests are welcome. `just check` must pass; the
integration suite additionally needs a throwaway GitHub organization —
see [docs/development/testing.md](docs/development/testing.md).

## License

MIT — see [LICENSE](LICENSE).
