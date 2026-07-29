package mapping

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Compile-time interface check.
var _ Store = (*SSM)(nil)

// Field names, one parameter each under the person's prefix.
//
// One parameter per field rather than a JSON blob per person: it is what
// makes per-field IAM possible, keeps a version history per field rather
// than per person, and means a partial write cannot corrupt an entry it did
// not touch.
const (
	fieldName       = "name"
	fieldGitHub     = "github"
	fieldK8s        = "k8s"
	fieldClass      = "class"
	fieldPinned     = "pinned"
	pinnedSeparator = ","
)

// SSM stores the mapping in AWS SSM Parameter Store.
//
// Layout, under the configured prefix:
//
//	/roster/<slug>/name     "First Last"
//	/roster/<slug>/github   "octocat"
//	/roster/<slug>/k8s      "flast"
//	/roster/<slug>/class    "employee"
//	/roster/<slug>/pinned   "truvity/robots,truvity/auditor"
//
// The path segment is a slug because a parameter name cannot contain a
// space; the display name is stored as its own parameter so nothing has to
// reverse the slug. Two people whose names slug identically collide — the
// same collision the "First Last" key already accepts, surfaced in one more
// place rather than a new one.
type SSM struct {
	client *ssm.Client
	prefix string
}

// NewSSM returns a store rooted at prefix, which must start and end with a
// slash (the config layer enforces that).
func NewSSM(client *ssm.Client, prefix string) *SSM {
	return &SSM{client: client, prefix: prefix}
}

// slugPattern is what survives as a path segment.
var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// Slug renders a display name as a path segment. Exported because the
// operator UI shows it: an operator who sees the slug can find the
// parameters.
func Slug(name string) string {
	return strings.Trim(slugPattern.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

func (s *SSM) personPath(name string) string { return s.prefix + Slug(name) + "/" }

// List reads every entry under the prefix in one paginated sweep.
func (s *SSM) List(ctx context.Context) ([]Entry, error) {
	byPerson := map[string]map[string]types.Parameter{}

	paginator := ssm.NewGetParametersByPathPaginator(s.client, &ssm.GetParametersByPathInput{
		Path:      aws.String(s.prefix),
		Recursive: aws.Bool(true),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list mapping under %q: %w", s.prefix, err)
		}

		for _, param := range page.Parameters {
			slug, field, ok := s.splitName(aws.ToString(param.Name))
			if !ok {
				// Something else lives under this prefix. Ignore it rather
				// than fail: the roster must not stop working because a
				// stray parameter was created next to it.
				continue
			}

			if byPerson[slug] == nil {
				byPerson[slug] = map[string]types.Parameter{}
			}

			byPerson[slug][field] = param
		}
	}

	entries := make([]Entry, 0, len(byPerson))

	for _, fields := range byPerson {
		entries = append(entries, entryFrom(fields))
	}

	return entries, nil
}

// Get reads one entry.
func (s *SSM) Get(ctx context.Context, name string) (Entry, error) {
	path := s.personPath(name)

	out, err := s.client.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path:      aws.String(path),
		Recursive: aws.Bool(true),
	})
	if err != nil {
		return Entry{}, fmt.Errorf("read mapping %q: %w", path, err)
	}

	if len(out.Parameters) == 0 {
		return Entry{}, ErrNotFound
	}

	fields := make(map[string]types.Parameter, len(out.Parameters))

	for _, param := range out.Parameters {
		if _, field, ok := s.splitName(aws.ToString(param.Name)); ok {
			fields[field] = param
		}
	}

	return entryFrom(fields), nil
}

// Put writes an entry, enforcing the invariants against everything stored.
//
// The read-then-write is not atomic — SSM has no transaction — so two
// operators editing at the same instant could both pass the uniqueness
// check. That is accepted: the window is milliseconds, the editor is used
// by a handful of people, and the audit record shows what happened. Making
// it atomic would mean a lock somewhere, which is a database by another
// name, and this service deliberately has none.
func (s *SSM) Put(ctx context.Context, entry Entry) error {
	existing, err := s.Get(ctx, entry.Name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	all, err := s.List(ctx)
	if err != nil {
		return err
	}

	if err := CheckInvariants(entry, existing, all); err != nil {
		return err
	}

	path := s.personPath(entry.Name)

	values := map[string]string{
		fieldName:   entry.Name,
		fieldGitHub: entry.GitHub,
		fieldClass:  string(entry.Class),
	}

	if entry.K8s != "" {
		values[fieldK8s] = entry.K8s
	}

	if len(entry.Pinned) > 0 {
		values[fieldPinned] = strings.Join(entry.Pinned, pinnedSeparator)
	}

	for field, value := range values {
		_, err := s.client.PutParameter(ctx, &ssm.PutParameterInput{
			Name:      aws.String(path + field),
			Value:     aws.String(value),
			Type:      types.ParameterTypeString,
			Overwrite: aws.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("write %q: %w", path+field, err)
		}
	}

	// Fields that are now empty must be removed, not left behind: a stale
	// `pinned` parameter would keep granting a team membership the operator
	// just took away.
	for _, field := range []string{fieldK8s, fieldPinned} {
		if _, keep := values[field]; keep {
			continue
		}

		if err := s.deleteParameter(ctx, path+field); err != nil {
			return err
		}
	}

	return nil
}

// Delete removes every parameter belonging to a person.
func (s *SSM) Delete(ctx context.Context, name string) error {
	path := s.personPath(name)

	for _, field := range []string{fieldName, fieldGitHub, fieldK8s, fieldClass, fieldPinned} {
		if err := s.deleteParameter(ctx, path+field); err != nil {
			return err
		}
	}

	return nil
}

func (s *SSM) deleteParameter(ctx context.Context, name string) error {
	_, err := s.client.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)})

	var notFound *types.ParameterNotFound
	if err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("delete %q: %w", name, err)
	}

	return nil
}

// splitName turns a full parameter name into its slug and field.
func (s *SSM) splitName(full string) (slug, field string, ok bool) {
	rest, found := strings.CutPrefix(full, s.prefix)
	if !found {
		return "", "", false
	}

	slug, field, found = strings.Cut(rest, "/")
	if !found || slug == "" || field == "" || strings.Contains(field, "/") {
		return "", "", false
	}

	return slug, field, true
}

// entryFrom assembles an entry from a person's parameters.
//
// A missing field yields its zero value rather than an error: a
// half-written person is a real state (a create interrupted between two
// PutParameter calls), and the join treats an incomplete entry as unusable
// anyway. Failing the whole listing would hide every other person instead.
func entryFrom(fields map[string]types.Parameter) Entry {
	entry := Entry{
		Name:   value(fields, fieldName),
		GitHub: value(fields, fieldGitHub),
		K8s:    value(fields, fieldK8s),
		Class:  Class(value(fields, fieldClass)),
	}

	if pinned := value(fields, fieldPinned); pinned != "" {
		entry.Pinned = strings.Split(pinned, pinnedSeparator)
	}

	// The revision shown is the highest across the person's parameters —
	// the version of whatever changed most recently, which is what an
	// operator means by "what version is this entry".
	var highest int64
	for _, param := range fields {
		if param.Version > highest {
			highest = param.Version
		}
	}

	if highest > 0 {
		entry.Revision = strconv.FormatInt(highest, 10)
	}

	return entry
}

func value(fields map[string]types.Parameter, field string) string {
	return aws.ToString(fields[field].Value)
}
