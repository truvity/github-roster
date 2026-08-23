// Package configstore holds structural configuration that operators can add
// alongside the git-delivered config document — the store layer under the
// IaC overlay (docs/architecture/reconciliation.md). This first slice covers
// directories; organizations and teams follow.
package configstore

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/truvity/github-roster/pkg/config"
)

// fields under each directory's prefix, one parameter each.
const (
	fieldEndpoint   = "endpoint"
	fieldDomains    = "domains" // comma-separated
	fieldProbeGroup = "probeGroup"
)

// DirectoryStore lists operator-added directories. Only resolver-backed
// directories are storable here (an endpoint, no in-process credential), so
// the store never holds a directory secret.
type DirectoryStore interface {
	List(ctx context.Context) ([]config.Source, error)
}

// SSM reads directories under <prefix>directories/<abbr>/.
type SSM struct {
	client *ssm.Client
	prefix string
}

// NewSSM roots a store at prefix (e.g. "/roster/"), reading its
// "directories/" segment.
func NewSSM(client *ssm.Client, prefix string) *SSM {
	return &SSM{client: client, prefix: prefix + "directories/"}
}

// List reads every stored directory in one paginated sweep. Malformed
// entries (no endpoint, no domains) are skipped rather than failing the
// read — a half-written directory must not take startup down.
func (s *SSM) List(ctx context.Context) ([]config.Source, error) {
	byName := map[string]map[string]string{}

	paginator := ssm.NewGetParametersByPathPaginator(s.client, &ssm.GetParametersByPathInput{
		Path:      aws.String(s.prefix),
		Recursive: aws.Bool(true),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list directories under %q: %w", s.prefix, err)
		}

		for i := range page.Parameters {
			name, field, ok := s.splitParam(aws.ToString(page.Parameters[i].Name))
			if !ok {
				continue
			}

			if byName[name] == nil {
				byName[name] = map[string]string{}
			}

			byName[name][field] = aws.ToString(page.Parameters[i].Value)
		}
	}

	return sourcesFrom(byName), nil
}

// splitParam parses "<prefix>directories/<abbr>/<field>" into (abbr, field).
func (s *SSM) splitParam(full string) (name, field string, ok bool) {
	rest := strings.TrimPrefix(full, s.prefix)
	if rest == full {
		return "", "", false
	}

	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}

	return parts[0], parts[1], true
}

// sourcesFrom turns the per-directory field maps into resolver-backed
// Sources, dropping any without an endpoint and domains.
func sourcesFrom(byName map[string]map[string]string) []config.Source {
	out := make([]config.Source, 0, len(byName))

	for name, fields := range byName {
		endpoint := fields[fieldEndpoint]
		domains := splitList(fields[fieldDomains])

		if endpoint == "" || len(domains) == 0 {
			continue
		}

		out = append(out, config.Source{
			Name:       name,
			Domains:    domains,
			Endpoint:   endpoint,
			ProbeGroup: fields[fieldProbeGroup],
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}

	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))

	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}

	return out
}

// MergeDirectories overlays the git-declared directories on top of the
// store: a git directory wins by name (case-insensitive), store-only
// directories are appended. Mirrors the people overlay's precedence.
func MergeDirectories(iac, store []config.Source) []config.Source {
	seen := make(map[string]bool, len(iac))
	for i := range iac {
		seen[strings.ToLower(iac[i].Name)] = true
	}

	out := append([]config.Source(nil), iac...)

	for i := range store {
		if seen[strings.ToLower(store[i].Name)] {
			continue // shadowed by git
		}

		out = append(out, store[i])
	}

	return out
}
