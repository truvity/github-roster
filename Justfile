# Development commands for github-roster

# Disable go.work (a parent workspace interferes with standalone module builds)
export GOWORK := "off"

# Format all Go files (gofmt + goimports via golangci-lint)
fmt:
    golangci-lint fmt ./...

# Rebuild the embedded TypeScript UI (frontend/dist). Run after changing
# anything under frontend/src; dist/ is committed so CI needs no Node.
build-ui:
    cd frontend && npm ci && npm run build

# Build the binary
build: fmt
    go build -o bin/github-roster ./cmd/github-roster/
    go build -o bin/roster-acceptance ./cmd/roster-acceptance/

# Run unit tests
test:
    go test ./... -coverprofile=coverage.out

# Run integration tests. Every group skips itself when its backend is absent,
# so this is safe to run locally with nothing configured — see docs/development/testing.md.
integration:
    go test -tags=integration -v -count=1 -timeout=600s ./tests/integration/...

# Render every chart values permutation (catches template breakage without a cluster).
# The exposure path is rendered too: those templates are only reachable with
# exposure.enabled, so a default-values render would never touch them.
chart-lint:
    helm lint charts/github-roster
    helm template github-roster charts/github-roster >/dev/null
    helm template github-roster charts/github-roster \
        --set exposure.enabled=true \
        --set exposure.hostname=roster.example.com \
        --set exposure.issuer=https://sso.example.com \
        --set exposure.jwksURI=https://sso.example.com/oauth/v2/keys \
        --set networkPolicy.enabled=true \
        --set config.oidc.roles.viewer=roster-viewers \
        --set config.oidc.roles.operator=roster-operators >/dev/null
    helm template github-roster charts/github-roster \
        --set audit.createBucket=true \
        --set audit.bucket=example-roster-audit \
        --set audit.retentionDays=400 >/dev/null

# Run linters
lint:
    golangci-lint run ./...

# Run the Go vulnerability check
vuln:
    govulncheck ./...

# Run go mod tidy
tidy:
    go mod tidy

# Clean build artifacts
clean:
    rm -rf bin/ dist/ coverage.out

# Everything CI runs on a pull request
check: build test lint chart-lint vuln

# Build a snapshot release locally (no push, no tag)
snapshot:
    goreleaser release --snapshot --clean

# Package the Helm chart locally
helm-package:
    helm package charts/github-roster --destination dist/

# ── Test installations ──────────────────────────────────────────────────
# One code path for an employee and for CI, per the shared-tier model
# (gitops docs/architecture/test-installations.md). The tenant identity is
# (NAMESPACE, RELEASE); every other name is DERIVED from it, so nothing can
# be configured twice and disagree.
#
#   employee:  just up dev-<abbr> github-roster
#   CI:        just e2e ci-<org> <repo>-r<run_id>-<attempt>
#
# Environment:
#   TEST_BUCKET       the shared test bucket (required for up)
#   TEST_SECRET_STORE the ClusterSecretStore materializing SSM secrets
#   TEST_ORG          the sandbox GitHub org (default truvity-roster-sandbox)
#   TEST_CREDS_SSM    credentials root (default /secrets/roster-test)
#   IMAGE_TAG         image + test-image tag (default latest)

# Install (or upgrade) one test installation. Idempotent: re-running is an
# upgrade in place, never a conflict.
up NAMESPACE RELEASE:
    #!/usr/bin/env bash
    set -euo pipefail
    NS='{{NAMESPACE}}' REL='{{RELEASE}}'
    # Helm stores the release in a Secret named sh.helm.release.v1.<name>.vN,
    # capping names at 53. Assert rather than discover in production.
    if [ "${#REL}" -gt 53 ]; then echo "release '$REL' exceeds helm's 53-character cap" >&2; exit 1; fi
    if ! [[ "$NS" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && "$REL" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
      echo "namespace and release must be DNS-1123 names" >&2; exit 1
    fi
    : "${TEST_BUCKET:?set TEST_BUCKET to the shared test bucket}"
    : "${TEST_SECRET_STORE:?set TEST_SECRET_STORE to the ClusterSecretStore name}"
    ORG="${TEST_ORG:-truvity-roster-sandbox}"
    CREDS="${TEST_CREDS_SSM:-/secrets/roster-test}"
    TAG="${IMAGE_TAG:-latest}"
    VALUES="$(mktemp)"; trap 'rm -f "$VALUES"' EXIT
    # Every tenant-scoped name below derives from (NS, REL) — the tenancy
    # invariant. State lives under /test/ (janitorable); credentials under
    # /secrets/ (mirror-owned, read-only here).
    cat > "$VALUES" <<VALUES_EOF
    image: {tag: "$TAG"}
    testImage: {tag: "$TAG"}
    applierSecrets:
      create: true
      secretStoreRef: {name: "$TEST_SECRET_STORE"}
    test:
      mutating: true
      executionID: ""
    config:
      oidc: {disabled: true}
      mapping: {ssmPrefix: /test/$NS/$REL/roster/}
      audit: {bucket: $TEST_BUCKET, prefix: $NS/$REL/, prefixPerOrg: true}
      reconciler:
        image: registry.k8s.io/prow/peribolos:latest
      companies:
        sandbox:
          directory:
            ssmPrefix: $CREDS/directory
            domains: [example.invalid]
          github:
            org: $ORG
            consoleAppSSM: $CREDS/console/$ORG
            applierAppSSM: $CREDS/applier/$ORG
            applierSecret: $REL-applier
    VALUES_EOF
    helm upgrade --install "$REL" charts/github-roster \
      --namespace "$NS" --create-namespace \
      --values "$VALUES" --wait --timeout 5m
    echo "up: $NS/$REL (state under /test/$NS/$REL/ and s3://$TEST_BUCKET/$NS/$REL/)"

# Run the chart's test hooks against a standing installation. selftest is
# read-only; the mutating acceptance suite runs because `up` set
# test.mutating=true — production values never do. (`test` alone is the
# unit suite; this one needs a cluster.)
test-install NAMESPACE RELEASE:
    helm test '{{RELEASE}}' --namespace '{{NAMESPACE}}' --logs

# Clear a tenant's own state for a clean slate. A convenience, never a
# prerequisite: the tests tolerate leftovers by execution-id scoping, and
# the janitor sweeps crashed runs.
reset NAMESPACE RELEASE:
    #!/usr/bin/env bash
    set -euo pipefail
    : "${TEST_BUCKET:?set TEST_BUCKET to the shared test bucket}"
    PREFIX="/test/{{NAMESPACE}}/{{RELEASE}}/"
    aws ssm get-parameters-by-path --path "$PREFIX" --recursive \
      --query 'Parameters[].Name' --output text | tr '\t' '\n' | grep -v '^$' \
      | while read -r name; do aws ssm delete-parameter --name "$name"; done || true
    aws s3 rm "s3://${TEST_BUCKET}/{{NAMESPACE}}/{{RELEASE}}/" --recursive || true
    echo "reset: state under $PREFIX and s3://$TEST_BUCKET/{{NAMESPACE}}/{{RELEASE}}/ cleared"

# Uninstall. State prefixes are left for `reset` or the janitor — the
# release disappearing is exactly what marks them as garbage.
down NAMESPACE RELEASE:
    helm uninstall '{{RELEASE}}' --namespace '{{NAMESPACE}}' --wait

# The CI shape: install, test, uninstall — one command, same path a human
# uses interactively.
e2e NAMESPACE RELEASE: (up NAMESPACE RELEASE) (test-install NAMESPACE RELEASE) (down NAMESPACE RELEASE)
