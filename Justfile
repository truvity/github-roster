# Development commands for github-roster

# Disable go.work (a parent workspace interferes with standalone module builds)
export GOWORK := "off"

# Format all Go files (gofmt + goimports via golangci-lint)
fmt:
    golangci-lint fmt ./...

# Build the binary
build: fmt
    go build -o bin/github-roster ./cmd/github-roster/

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
