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

## Directory data is fixtures, always

Liveness and group membership come from fixtures in every test. Reading a
real corporate directory from CI would mean holding directory credentials
in CI, and the join logic does not care where the rows came from.
