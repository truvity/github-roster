package mapping

import (
	"context"
	"fmt"
	"strings"
)

// ErrManagedInGit is returned when a write targets an entry declared in the
// IaC layer. Those are edited by a reviewed commit, not the operator UI.
var ErrManagedInGit = fmt.Errorf("entry is managed in git")

// gitRevision marks an overlay (IaC) entry's provenance in its Revision
// field, so the console can show it as git-managed and read-only.
const gitRevision = "git"

// Overlay merges an IaC layer of entries on top of an underlying Store: the
// git-declared entries win by name, and writes to them are refused
// (ErrManagedInGit). Entries not named by the IaC layer pass through to the
// inner store unchanged. This is how an installation keeps some or all of
// the mapping under reviewed pull requests while operators edit the rest.
type Overlay struct {
	inner Store
	// iac is keyed by lower-cased name.
	iac map[string]Entry
}

// NewOverlay wraps inner with the given IaC entries. The entries are
// validated up front — a malformed git-declared entry is a startup failure,
// not a silent surprise at read time.
func NewOverlay(inner Store, iac []Entry) (*Overlay, error) {
	m := make(map[string]Entry, len(iac))

	for i := range iac {
		e := iac[i]
		if err := ValidateEntry(e); err != nil {
			return nil, fmt.Errorf("iac people: %q: %w", e.Name, err)
		}

		key := strings.ToLower(e.Name)
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("iac people: %q declared twice", e.Name)
		}

		e.Revision = gitRevision
		m[key] = e
	}

	return &Overlay{inner: inner, iac: m}, nil
}

func (o *Overlay) managed(name string) bool {
	_, ok := o.iac[strings.ToLower(name)]

	return ok
}

// List returns the inner entries with the IaC layer merged on top: a
// git-declared name replaces the inner entry, and git-only names are added.
func (o *Overlay) List(ctx context.Context) ([]Entry, error) {
	inner, err := o.inner.List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Entry, 0, len(inner)+len(o.iac))

	for i := range inner {
		if o.managed(inner[i].Name) {
			continue // shadowed by the IaC layer
		}

		out = append(out, inner[i])
	}

	for key := range o.iac {
		out = append(out, o.iac[key])
	}

	return out, nil
}

// Get prefers the IaC layer, then the inner store.
func (o *Overlay) Get(ctx context.Context, name string) (Entry, error) {
	if e, ok := o.iac[strings.ToLower(name)]; ok {
		return e, nil
	}

	return o.inner.Get(ctx, name)
}

// Put refuses a git-managed name; otherwise delegates.
func (o *Overlay) Put(ctx context.Context, entry Entry) error {
	if o.managed(entry.Name) {
		return fmt.Errorf("%w: %q", ErrManagedInGit, entry.Name)
	}

	return o.inner.Put(ctx, entry)
}

// Delete refuses a git-managed name; otherwise delegates.
func (o *Overlay) Delete(ctx context.Context, name string) error {
	if o.managed(name) {
		return fmt.Errorf("%w: %q", ErrManagedInGit, name)
	}

	return o.inner.Delete(ctx, name)
}
