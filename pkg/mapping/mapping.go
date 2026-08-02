// Package mapping is the person → identity store: "First Last" to a GitHub
// handle and a Kubernetes-safe abbreviation.
//
// The store is an interface with more than one implementation on purpose.
// Today the entries live in AWS SSM Parameter Store, which is a good fit —
// per-parameter IAM, versioning for free, no server to run — but nothing
// above this package knows that. Swapping it for a different backing store
// is a new implementation of [Store] and a line in the wiring, not a change
// to the join, the UI, or the reconciler.
//
// Reads and writes are separate interfaces. Most of the service only ever
// reads, and a [Reader] cannot be handed to something that then writes
// through it by accident.
package mapping

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Class decides whether directory liveness governs an entry.
type Class string

const (
	// ClassEmployee is governed by directory liveness: suspend the person
	// in the directory and the next scheduled run removes them.
	ClassEmployee Class = "employee"
	// ClassBot has no directory account and no liveness signal. Bots are
	// removed only by an operator deleting the entry.
	//
	// An `external` class is a documented later concept, deliberately not
	// built yet: nobody has needed it, and a class that exists without a
	// rule attached is a class people guess about.
	ClassBot Class = "bot"
)

// Valid reports whether the class is one this service knows.
func (c Class) Valid() bool { return c == ClassEmployee || c == ClassBot }

// Entry is one person's mapping.
type Entry struct {
	// Name is the primary key: the person's display name, "First Last".
	//
	// It is a deliberate choice with a known sharp edge — two people with
	// identical names collide. Email would be a better key, but a person
	// can exist in several directories under several addresses, and the
	// name is the only identifier every source shares.
	Name string `json:"name"`

	// GitHub is the person's GitHub login. Entered by an operator; never
	// self-service, never guessed from an email address.
	GitHub string `json:"github"`

	// Emails are the addresses the directories know this person under —
	// the explicit IdP connection, one address per identity (a person
	// spanning companies has several). The join matches on these FIRST
	// and falls back to the name, so a directory spelling a name
	// differently cannot silently detach a person from their liveness.
	Emails []string `json:"emails,omitempty"`

	// K8s is the abbreviation their `emp-<abbr>` namespace is named after.
	// DNS-1123, unique, and immutable once assigned — renaming it would
	// orphan a live namespace.
	K8s string `json:"k8s"`

	// Class governs whether liveness applies.
	Class Class `json:"class"`

	// Pinned lists pinned team memberships as "<org>/<team>". Pinned teams
	// have no directory group to read, so this is where their membership
	// lives.
	Pinned []string `json:"pinned,omitempty"`

	// Revision is the store's opaque version for this entry, for display
	// and for optimistic concurrency. Empty when a store has no versioning.
	Revision string `json:"revision,omitempty"`
}

// Reader is the read half of the store. Most of the service needs only this.
type Reader interface {
	// List returns every entry, in no guaranteed order.
	List(ctx context.Context) ([]Entry, error)
	// Get returns one entry, or ErrNotFound.
	Get(ctx context.Context, name string) (Entry, error)
}

// Store adds the write half, used only by the operator-facing editor.
type Store interface {
	Reader

	// Put creates or replaces an entry. Implementations must reject a
	// change that violates the invariants — see [ValidateEntry] and
	// [CheckInvariants]; enforcing them in the store is what keeps a
	// second caller from bypassing the UI's checks.
	Put(ctx context.Context, entry Entry) error
	// Delete removes an entry. Deleting an absent entry is not an error:
	// the caller wanted it gone, and it is gone.
	Delete(ctx context.Context, name string) error
}

// Errors a store may return.
var (
	// ErrNotFound is returned by Get for an entry that does not exist.
	ErrNotFound = errors.New("mapping entry not found")
	// ErrImmutable is returned when a write would change a field that is
	// immutable once assigned.
	ErrImmutable = errors.New("field is immutable once assigned")
	// ErrDuplicate is returned when a write would break uniqueness.
	ErrDuplicate = errors.New("value is already taken")
)

// dns1123 is the shape a Kubernetes abbreviation must have.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// MaxAbbrevLen caps the abbreviation.
//
// The abbreviation is a prefix of object names — `emp-<abbr>` namespaces,
// and release names inside them — and Kubernetes caps several of those at
// 63 characters. 20 leaves room for whatever gets appended without anyone
// having to think about it again.
const MaxAbbrevLen = 20

// githubLoginChars is the character set and length GitHub allows.
// The "no doubled, leading or trailing hyphen" half of the rule is checked
// in code: Go's regexp is RE2 and has no lookahead, and spelling it out is
// clearer than the alternation that would be needed to encode it.
var githubLoginChars = regexp.MustCompile(`^[a-zA-Z0-9-]{1,39}$`)

// validGitHubLogin reports whether a string is a possible GitHub username.
func validGitHubLogin(login string) bool {
	return githubLoginChars.MatchString(login) &&
		!strings.HasPrefix(login, "-") &&
		!strings.HasSuffix(login, "-") &&
		!strings.Contains(login, "--")
}

// ValidateEntry checks one entry in isolation.
func ValidateEntry(e Entry) error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("name is required")
	}

	if e.Name != strings.TrimSpace(e.Name) {
		// Leading or trailing space makes two entries that look identical
		// and join differently — the worst kind of duplicate.
		return fmt.Errorf("name %q has leading or trailing whitespace", e.Name)
	}

	if !e.Class.Valid() {
		return fmt.Errorf("class %q must be %q or %q", e.Class, ClassEmployee, ClassBot)
	}

	if e.GitHub == "" {
		return fmt.Errorf("github login is required")
	}

	if !validGitHubLogin(e.GitHub) {
		return fmt.Errorf("github login %q is not a valid GitHub username", e.GitHub)
	}

	for _, email := range e.Emails {
		if email != strings.ToLower(strings.TrimSpace(email)) {
			return fmt.Errorf("email %q must be lowercase with no surrounding whitespace", email)
		}

		at := strings.Index(email, "@")
		if at <= 0 || at == len(email)-1 || strings.ContainsAny(email, " \t") {
			return fmt.Errorf("email %q is not a plausible address", email)
		}
	}

	// The abbreviation is optional: a bot has no namespace, and a person
	// can be mapped for GitHub before anyone decides on their namespace.
	if e.K8s != "" {
		if !dns1123.MatchString(e.K8s) {
			return fmt.Errorf("k8s abbreviation %q must be lowercase alphanumeric with dashes (DNS-1123)", e.K8s)
		}

		if len(e.K8s) > MaxAbbrevLen {
			return fmt.Errorf("k8s abbreviation %q is longer than %d characters", e.K8s, MaxAbbrevLen)
		}
	}

	for _, pinned := range e.Pinned {
		if _, _, ok := strings.Cut(pinned, "/"); !ok {
			return fmt.Errorf("pinned team %q must be %q", pinned, "<org>/<team>")
		}
	}

	return nil
}

// CheckInvariants validates a write against everything already stored: the
// cross-entry rules that a single entry cannot express.
//
// existing is the entry being replaced, or the zero Entry for a create.
func CheckInvariants(next, existing Entry, all []Entry) error {
	if err := ValidateEntry(next); err != nil {
		return err
	}

	// Immutable once assigned — and only once assigned. Filling in a blank
	// abbreviation is a normal edit; changing one orphans a live namespace
	// whose name nothing else will ever update.
	if existing.K8s != "" && next.K8s != existing.K8s {
		return fmt.Errorf("%w: k8s abbreviation is %q, cannot become %q", ErrImmutable, existing.K8s, next.K8s)
	}

	for i := range all {
		other := &all[i]

		if other.Name == next.Name {
			continue
		}

		if next.K8s != "" && strings.EqualFold(other.K8s, next.K8s) {
			return fmt.Errorf("%w: k8s abbreviation %q belongs to %q", ErrDuplicate, next.K8s, other.Name)
		}

		// GitHub logins are case-insensitive, and two people mapped to one
		// account makes the join ambiguous in a way no later check catches.
		if strings.EqualFold(other.GitHub, next.GitHub) {
			return fmt.Errorf("%w: github login %q belongs to %q", ErrDuplicate, next.GitHub, other.Name)
		}
	}

	return nil
}
