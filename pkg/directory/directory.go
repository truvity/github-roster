// Package directory reads corporate directories: who exists, who is
// suspended, and who is in which group.
//
// This is the liveness half of the join. Everything else the service does
// depends on getting one question right — "is this person still with the
// company?" — and on getting it wrong in the safe direction when the answer
// is unavailable.
//
// A [Source] is one directory. The interface exists so the service is not
// married to any single vendor: when the identity waist lands, a new source
// type is a new implementation here and a row in the config, and nothing
// above this package changes.
package directory

import (
	"context"
	"strings"
	"time"
)

// User is one person in a directory.
type User struct {
	// Name is "First Last", built from the directory's given and family
	// name. It is the join key, so it is built the same way for everyone
	// rather than taken from a display-name field that people edit.
	Name string `json:"name"`

	// Email is the person's primary address, for display and for matching
	// group membership.
	Email string `json:"email"`

	// Live is false for a suspended account. Suspension is the
	// authoritative "no longer with us" signal: it is what an offboarding
	// runbook does first, and it takes effect everywhere at once.
	Live bool `json:"live"`
}

// Snapshot is one directory read, as of one moment.
type Snapshot struct {
	// Source is the configured name of the directory this came from.
	Source string `json:"source"`

	// Users are everyone in the configured domains.
	Users []User `json:"users"`

	// Groups maps a group's address to its member addresses.
	//
	// Flat: a nested group is not expanded. A team whose membership you
	// cannot determine from one group listing is not a team anyone can
	// review, and the nested family is being retired for exactly that
	// reason.
	Groups map[string][]string `json:"groups"`

	// FetchedAt is when this read completed.
	FetchedAt time.Time `json:"fetchedAt"`
}

// Source is one directory.
type Source interface {
	// Name is the source's configured name.
	Name() string
	// Fetch reads the whole directory. Implementations must return an
	// error rather than a partial snapshot: a snapshot missing half its
	// users reads exactly like half the company leaving.
	Fetch(ctx context.Context) (*Snapshot, error)
}

// LiveUsers returns the users who are not suspended.
func (s *Snapshot) LiveUsers() []User {
	live := make([]User, 0, len(s.Users))

	for _, u := range s.Users {
		if u.Live {
			live = append(live, u)
		}
	}

	return live
}

// GroupMembers returns the union of the named groups' members, deduplicated
// and lowercased.
//
// The union, not the intersection: a team drawn from two groups means
// "anyone in either", which is how people actually describe them.
func (s *Snapshot) GroupMembers(groups []string) []string {
	seen := map[string]bool{}
	members := []string{}

	for _, group := range groups {
		for _, email := range s.Groups[strings.ToLower(group)] {
			email = strings.ToLower(email)
			if seen[email] {
				continue
			}

			seen[email] = true

			members = append(members, email)
		}
	}

	return members
}

// UserByEmail finds a user by address, case-insensitively.
func (s *Snapshot) UserByEmail(email string) (User, bool) {
	for _, u := range s.Users {
		if strings.EqualFold(u.Email, email) {
			return u, true
		}
	}

	return User{}, false
}

// FullName builds the join key from a directory's name parts.
//
// Given and family name rather than the directory's own full-name field:
// the full name is free text that people edit into "Bob (Robert) Smith" and
// worse, while the parts are what onboarding fills in. A directory that has
// only a full name still works — the fallback keeps it.
func FullName(given, family, full string) string {
	given, family = strings.TrimSpace(given), strings.TrimSpace(family)

	if given != "" && family != "" {
		return given + " " + family
	}

	if full = strings.TrimSpace(full); full != "" {
		return full
	}

	// One part is better than nothing: it will not join, but it shows up in
	// the UI as an unmapped person rather than vanishing.
	return strings.TrimSpace(given + " " + family)
}
