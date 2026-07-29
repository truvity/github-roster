package directory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"
)

// Compile-time interface check.
var _ Source = (*Google)(nil)

// Google reads a Google Workspace directory through the Admin SDK.
//
// Authentication is domain-wide delegation: a service-account key signing
// for an administrator. The delegation grant deliberately covers only the
// two read-only scopes below — a token request including anything else is
// rejected outright, which is a feature.
type Google struct {
	name    string
	domains []string
	groups  []string

	keyJSON []byte
	subject string
}

// GoogleConfig is what one Workspace source needs.
type GoogleConfig struct {
	// Name is the source's configured name.
	Name string
	// Domains restricts the read. Required: a Workspace may serve domains
	// this instance has no business managing, and reading them would
	// import people it must never act on.
	Domains []string
	// Groups are the group addresses to resolve. Only the groups the
	// configuration actually maps to teams are fetched — listing every
	// group in the Workspace would be slower and would read membership the
	// service has no use for.
	Groups []string

	// KeyJSON is the service-account key.
	KeyJSON []byte
	// Subject is the administrator to impersonate.
	Subject string
}

// NewGoogle builds a Workspace source.
func NewGoogle(cfg GoogleConfig) (*Google, error) {
	switch {
	case cfg.Name == "":
		return nil, fmt.Errorf("directory source name is required")
	case len(cfg.Domains) == 0:
		return nil, fmt.Errorf("source %q: at least one domain is required", cfg.Name)
	case len(cfg.KeyJSON) == 0:
		return nil, fmt.Errorf("source %q: service account key is required", cfg.Name)
	case cfg.Subject == "":
		return nil, fmt.Errorf("source %q: admin subject to impersonate is required", cfg.Name)
	}

	return &Google{
		name:    cfg.Name,
		domains: cfg.Domains,
		groups:  cfg.Groups,
		keyJSON: cfg.KeyJSON,
		subject: cfg.Subject,
	}, nil
}

// Name returns the source's configured name.
func (g *Google) Name() string { return g.name }

// fetchTimeout bounds one whole directory read.
const fetchTimeout = 2 * time.Minute

// Fetch reads users for every configured domain and members for every
// configured group.
func (g *Google) Fetch(ctx context.Context) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	svc, err := g.service(ctx)
	if err != nil {
		return nil, err
	}

	snapshot := &Snapshot{Source: g.name, Groups: map[string][]string{}}

	for _, domain := range g.domains {
		users, err := g.usersIn(ctx, svc, domain)
		if err != nil {
			return nil, err
		}

		snapshot.Users = append(snapshot.Users, users...)
	}

	for _, group := range g.groups {
		members, err := g.membersOf(ctx, svc, group)
		if err != nil {
			return nil, err
		}

		snapshot.Groups[strings.ToLower(group)] = members
	}

	snapshot.FetchedAt = time.Now()

	return snapshot, nil
}

func (g *Google) service(ctx context.Context) (*admin.Service, error) {
	// Exactly the two scopes the delegation grants. Asking for more fails
	// the token request outright rather than degrading.
	jwtConfig, err := google.JWTConfigFromJSON(g.keyJSON,
		admin.AdminDirectoryUserReadonlyScope,
		admin.AdminDirectoryGroupReadonlyScope,
	)
	if err != nil {
		// Deliberately does not include the key material in the error.
		return nil, fmt.Errorf("source %q: parse service account key: %w", g.name, err)
	}

	jwtConfig.Subject = g.subject

	svc, err := admin.NewService(ctx, option.WithHTTPClient(jwtConfig.Client(ctx)))
	if err != nil {
		return nil, fmt.Errorf("source %q: create directory service: %w", g.name, err)
	}

	return svc, nil
}

// usersIn lists one domain's users.
func (g *Google) usersIn(ctx context.Context, svc *admin.Service, domain string) ([]User, error) {
	var users []User

	call := svc.Users.List().Domain(domain).MaxResults(500)

	err := call.Pages(ctx, func(page *admin.Users) error {
		for _, u := range page.Users {
			users = append(users, userFrom(u))
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: list users in %q: %w", g.name, domain, err)
	}

	return users, nil
}

// membersOf lists one group's direct members.
//
// Direct, not transitive: nested groups are deliberately not expanded.
func (g *Google) membersOf(ctx context.Context, svc *admin.Service, group string) ([]string, error) {
	members := []string{}

	err := svc.Members.List(group).MaxResults(500).Pages(ctx, func(page *admin.Members) error {
		for _, m := range page.Members {
			// A member entry can be a nested group; those are skipped
			// rather than expanded, and skipping them silently is fine
			// because the config documents that groups are read flat.
			if strings.EqualFold(m.Type, "GROUP") {
				continue
			}

			if m.Email != "" {
				members = append(members, strings.ToLower(m.Email))
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: list members of %q: %w", g.name, group, err)
	}

	return members, nil
}

func userFrom(u *admin.User) User {
	var given, family, full string
	if u.Name != nil {
		given, family, full = u.Name.GivenName, u.Name.FamilyName, u.Name.FullName
	}

	return User{
		Name:  FullName(given, family, full),
		Email: strings.ToLower(u.PrimaryEmail),
		// Suspended is the authoritative not-live signal. Archived and
		// unlicensed accounts are deliberately NOT treated as gone: they
		// are billing states, not employment states, and reading them as
		// departures would remove people who are merely on leave.
		Live: !u.Suspended,
	}
}
