# Testing

Three layers, with sharply different requirements.

## Unit tests — `just test`

No network, no credentials, no containers. They cover the parts where a
mistake is silent and expensive: the join (an unmapped joiner, a stale
mapping entry, a suspended user), config rendering as golden files, the
mapping invariants, and the removals-only rendering asymmetry.

These run on every pull request, including from forks.

## Integration tests — `just integration`

Built with the `integration` tag. They exercise the real thing wherever a
fake would test the wrong system:

| Group | Backend | Skipped when |
|---|---|---|
| GitHub | a **real, throwaway** organization and a real GitHub App pair | `ROSTER_TEST_ORG` is unset |
| SSM / S3 | localstack | `AWS_ENDPOINT_URL` is unset |
| Jobs | a kind cluster | `ROSTER_TEST_KUBECONFIG` is unset |

Running the whole file with nothing configured is therefore a no-op rather
than a failure — a fresh clone can run `just integration` and see skips.

**How CI gets its credentials.** Repository **variables** carry the
identifiers (organization, App ids, installation ids) and repository
**secrets** carry only the two private keys. No password manager is
involved in CI, and nothing reaches out to a cloud account.

The split is not carelessness about the identifiers. A secret is masked
as `***` wherever it appears, so carrying the org name and App ids as
secrets turns every diagnostic line into `org "***"` — precisely the
information you need when an integration test fails. None of them
identify anything sensitive: the organization is a free, throwaway
account whose only purpose is being mutated by tests, and an App id is
meaningless without the key.

**Why a real organization.** GitHub's invitation lifecycle is the single
most error-prone part of this service's problem domain: an invited-but-not-
accepted user is a member for some purposes and not others. A mock would
encode our beliefs about that behavior and then confirm them. The tests
create teams, invite, reconcile and remove against a throwaway org whose
only purpose is being mutated.

**Collisions.** Every object a test creates is tagged with
`ROSTER_TEST_RUN_ID`, so concurrent CI runs cannot collide, and anything
left behind is attributable to a run. Tests clean up after themselves.

### Running them locally

You need your own throwaway organization and your own App pair — never point
these at an organization with real people in it.

```bash
export ROSTER_TEST_RUN_ID=local-$USER
export ROSTER_TEST_ORG=my-throwaway-org
export ROSTER_TEST_CONSOLE_APP_ID=... ROSTER_TEST_CONSOLE_INSTALLATION_ID=...
export ROSTER_TEST_CONSOLE_PRIVATE_KEY="$(cat console.pem)"
export ROSTER_TEST_APPLIER_APP_ID=... ROSTER_TEST_APPLIER_INSTALLATION_ID=...
export ROSTER_TEST_APPLIER_PRIVATE_KEY="$(cat applier.pem)"

docker run -d -p 4566:4566 -e SERVICES=ssm,s3 localstack/localstack:4
export AWS_ENDPOINT_URL=http://localhost:4566 AWS_REGION=eu-west-1 \
       AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test

just integration
```

The App credentials belong in your secret manager, not in your shell
history or a `.env` file. A private key that has been pasted somewhere is a
key you should rotate.

### The privilege split is part of the test

The console App is read-only and the applier App is write-capable, in the
test environment exactly as in production. A test suite that used one
all-powerful credential would exercise a privilege boundary that does not
exist, and would pass on the day the real boundary broke.

## Against a real cluster — `just up / test-install / down`

The deployment-verification tier: install the chart into a live cluster
and run its own test hooks against the running release. One code path for
a person and for CI — the same recipes, differing only in arguments:

```bash
# a person, in their own namespace
just up dev-ada github-roster
just test-install dev-ada github-roster
just down dev-ada github-roster

# CI, one ephemeral release per run in a shared namespace
just e2e ci-myorg myrepo-r12345-1
```

The pair `(NAMESPACE, RELEASE)` is the tenant identity, and every other
name derives from it: state lives under `/test/<ns>/<release>/` in the
parameter store and `<ns>/<release>/` in the shared bucket, and the
applier Secret is `<release>-applier`. Nothing is configured twice, so
nothing can disagree. Re-running `up` is an upgrade in place; re-running
tests tolerates leftovers because every entity a test creates carries an
execution id. `reset` clears a tenant's own prefixes when a human wants a
clean slate — a convenience, never a prerequisite.

Required environment: `TEST_BUCKET` (the shared test bucket),
`TEST_SECRET_STORE` (the External Secrets store materializing the applier
key), and optionally `TEST_ORG` / `TEST_CREDS_SSM` / `IMAGE_TAG`.

`helm test` runs two hooks from the dedicated test image:

| Hook | Mutates | Exists when |
|---|---|---|
| `selftest` | no — wiring checks only (IAM, KMS, RBAC, credentials) | always, every environment |
| `acceptance` | sandbox only, execution-scoped, cleaned up | only when values set `test.mutating=true` — which the `up` recipe does and production values never do |

## Directory data is fixtures, always

Liveness and group membership come from fixtures in every test. Reading a
real corporate directory from CI would mean holding directory credentials
in CI, and the join logic does not care where the rows came from.
