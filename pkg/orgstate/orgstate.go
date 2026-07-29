// Package orgstate reads a GitHub organization: who is a member, which
// teams exist, who is in them, and who has been invited but has not
// accepted.
//
// Reading only. Every write in this service goes through a reconciler Job
// holding a different credential, and this package is used by the web tier,
// which holds the read-only one. There is no write method here to call by
// mistake.
package orgstate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v76/github"

	"github.com/truvity/github-roster/pkg/githubapp"
)

// Role is a member's organization role.
type Role string

const (
	// RoleAdmin is an organization owner. Owners are explicitly not this
	// service's business — they are registry-pinned and change by reviewed
	// infrastructure commits — but they must be read, because a
	// reconciler config that omitted them would propose removing them.
	RoleAdmin Role = "admin"
	// RoleMember is an ordinary member.
	RoleMember Role = "member"
)

// Member is one organization member.
type Member struct {
	Login string `json:"login"`
	Role  Role   `json:"role"`
}

// Invitation is a membership invitation that has been sent and not yet
// accepted.
//
// These matter more than their obscurity suggests. An invited person is a
// member for some purposes and not others: they occupy a seat, they appear
// in the organization's people list, and they are absent from the members
// API. Treating them as "not a member" causes a second invitation; treating
// them as a member causes a removal that cancels their pending invite. Both
// are visible to the person on the other end.
type Invitation struct {
	// Login is empty for an invitation sent to an email address that has
	// no GitHub account yet.
	Login     string    `json:"login,omitempty"`
	Email     string    `json:"email,omitempty"`
	Role      string    `json:"role,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Team is one team.
type Team struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// State is everything this service reads about one organization, as of one
// moment.
type State struct {
	Org         string       `json:"org"`
	Members     []Member     `json:"members"`
	Invitations []Invitation `json:"invitations"`
	Teams       []Team       `json:"teams"`
	// TeamMembers is keyed by team slug.
	TeamMembers map[string][]string `json:"teamMembers"`
	// ReadAt is when the read completed, so the UI can say how old it is.
	ReadAt time.Time `json:"readAt"`
}

// IsMember reports whether a login is a member or has a pending invitation.
// Case-insensitive, because GitHub logins are.
func (s *State) IsMember(login string) bool {
	for _, m := range s.Members {
		if strings.EqualFold(m.Login, login) {
			return true
		}
	}

	for _, i := range s.Invitations {
		if i.Login != "" && strings.EqualFold(i.Login, login) {
			return true
		}
	}

	return false
}

// Reader reads one organization with the console App's credentials.
type Reader struct {
	client *github.Client
	org    string
}

// perPage is the maximum GitHub allows, so a hundred-person organization is
// one request per resource rather than four.
const perPage = 100

// NewReader builds a reader for one organization.
func NewReader(source *githubapp.TokenSource, org, baseURL string) (*Reader, error) {
	if org == "" {
		return nil, fmt.Errorf("organization is required")
	}

	client := github.NewClient(&http.Client{
		Transport: &tokenTransport{source: source},
		Timeout:   requestTimeout,
	})

	if baseURL != "" {
		var err error

		client, err = client.WithEnterpriseURLs(baseURL, baseURL)
		if err != nil {
			return nil, fmt.Errorf("set api base %q: %w", baseURL, err)
		}
	}

	return &Reader{client: client, org: org}, nil
}

const requestTimeout = 30 * time.Second

// tokenTransport attaches a fresh installation token to every request.
//
// Per request rather than per client because installation tokens expire
// after an hour, and this process outlives that comfortably.
type tokenTransport struct {
	source *githubapp.TokenSource
	base   http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.source.Token(req.Context())
	if err != nil {
		return nil, err
	}

	// Clone: a RoundTripper must not modify the request it is given.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(clone)
}

// Read fetches the whole organization state.
//
// Sequentially, not in parallel: GitHub's own guidance for github.com is to
// serialize writes and be gentle with reads, the whole read is a handful of
// requests, and a page that loads in 400ms instead of 200ms is not worth a
// concurrency bug in code that decides who keeps access.
func (r *Reader) Read(ctx context.Context) (*State, error) {
	state := &State{Org: r.org, TeamMembers: map[string][]string{}}

	var err error

	if state.Members, err = r.Members(ctx); err != nil {
		return nil, err
	}

	if state.Invitations, err = r.PendingInvitations(ctx); err != nil {
		return nil, err
	}

	if state.Teams, err = r.Teams(ctx); err != nil {
		return nil, err
	}

	for _, team := range state.Teams {
		members, err := r.TeamMembers(ctx, team.Slug)
		if err != nil {
			return nil, err
		}

		state.TeamMembers[team.Slug] = members
	}

	state.ReadAt = time.Now()

	return state, nil
}

// Members lists organization members with their roles.
func (r *Reader) Members(ctx context.Context) ([]Member, error) {
	var members []Member

	// Roles come from two separate listings: GitHub's members endpoint
	// filters by role rather than reporting it.
	for _, role := range []Role{RoleAdmin, RoleMember} {
		opts := &github.ListMembersOptions{
			Role:        string(role),
			ListOptions: github.ListOptions{PerPage: perPage},
		}

		for {
			page, resp, err := r.client.Organizations.ListMembers(ctx, r.org, opts)
			if err != nil {
				return nil, fmt.Errorf("list %s members of %q: %w", role, r.org, err)
			}

			for _, user := range page {
				members = append(members, Member{Login: user.GetLogin(), Role: role})
			}

			if resp.NextPage == 0 {
				break
			}

			opts.Page = resp.NextPage
		}
	}

	return members, nil
}

// PendingInvitations lists invitations that have been sent and not accepted.
func (r *Reader) PendingInvitations(ctx context.Context) ([]Invitation, error) {
	var invitations []Invitation

	opts := &github.ListOptions{PerPage: perPage}

	for {
		page, resp, err := r.client.Organizations.ListPendingOrgInvitations(ctx, r.org, opts)
		if err != nil {
			return nil, fmt.Errorf("list pending invitations of %q: %w", r.org, err)
		}

		for _, invite := range page {
			invitations = append(invitations, Invitation{
				Login:     invite.GetLogin(),
				Email:     invite.GetEmail(),
				Role:      invite.GetRole(),
				CreatedAt: invite.GetCreatedAt().Time,
			})
		}

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return invitations, nil
}

// Teams lists the organization's teams.
func (r *Reader) Teams(ctx context.Context) ([]Team, error) {
	var teams []Team

	opts := &github.ListOptions{PerPage: perPage}

	for {
		page, resp, err := r.client.Teams.ListTeams(ctx, r.org, opts)
		if err != nil {
			return nil, fmt.Errorf("list teams of %q: %w", r.org, err)
		}

		for _, team := range page {
			teams = append(teams, Team{
				ID:   team.GetID(),
				Slug: team.GetSlug(),
				Name: team.GetName(),
			})
		}

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return teams, nil
}

// TeamMembers lists a team's members by login.
func (r *Reader) TeamMembers(ctx context.Context, slug string) ([]string, error) {
	logins := []string{}

	opts := &github.TeamListTeamMembersOptions{ListOptions: github.ListOptions{PerPage: perPage}}

	for {
		page, resp, err := r.client.Teams.ListTeamMembersBySlug(ctx, r.org, slug, opts)
		if err != nil {
			return nil, fmt.Errorf("list members of team %q in %q: %w", slug, r.org, err)
		}

		for _, user := range page {
			logins = append(logins, user.GetLogin())
		}

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return logins, nil
}
