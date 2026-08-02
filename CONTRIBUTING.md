# Contributing

Bug reports and pull requests are welcome.

## Development environment

```bash
devbox shell          # or: install Go, just, helm, golangci-lint
just check            # build + unit tests + lint + chart render
```

`just check` must pass before a pull request; a lefthook pre-push hook
runs the same gate locally. The integration suite additionally needs a
throwaway GitHub organization — see
[docs/development/testing.md](docs/development/testing.md).

## What lands where

- **Membership behavior** (join, guards, reconciliation) — this repo.
- **Team creation/deletion, repositories, org settings** — deliberately
  out of scope; that is structure, owned by whatever infrastructure-as-code
  engine manages your organization. The service reconciles membership of
  teams that already exist.

## Style

- Server-rendered HTML, no SPA, no front-end build step. Static assets
  are plain files under `pkg/ui/static/`; the CSP allows no inline
  scripts.
- Comments state constraints the code cannot (see the failure-semantics
  notes in `docs/architecture/overview.md`); they are not narration.
- Every guard gets a test that proves the unsafe path is closed.

## Releases

Tag `vX.Y.Z` on master. CI publishes the image to
`ghcr.io/truvity/github-roster` and the chart to
`oci://ghcr.io/truvity/charts/github-roster`. One-line summary in the
tag annotation; add the line to [CHANGELOG.md](CHANGELOG.md).
