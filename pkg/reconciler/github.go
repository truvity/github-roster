package reconciler

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v90/github"

	"github.com/truvity/github-roster/pkg/githubapp"
)

// writeTimeout bounds one GitHub write. Generous: an invitation triggers
// e-mail delivery on GitHub's side and is their slowest membership call.
const writeTimeout = 30 * time.Second

// GitHubWriter performs the plan against the real API, authenticated as
// the applier App. It is constructed inside the Job and nowhere else.
type GitHubWriter struct {
	client *github.Client
}

// NewGitHubWriter builds a writer from the applier App's credentials.
func NewGitHubWriter(source *githubapp.TokenSource) (*GitHubWriter, error) {
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{
		Transport: &appTransport{source: source},
		Timeout:   writeTimeout,
	}))
	if err != nil {
		return nil, fmt.Errorf("build github client: %w", err)
	}

	return &GitHubWriter{client: client}, nil
}

// appTransport attaches a fresh installation token to every request, the
// same way the read side does: per request, because installation tokens
// expire after an hour.
type appTransport struct {
	source *githubapp.TokenSource
	base   http.RoundTripper
}

func (t *appTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.source.Token(req.Context())
	if err != nil {
		return nil, err
	}

	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(clone)
}

// SetMembership adds, invites, or changes the role of a login.
//
// One endpoint covers all three on GitHub's side: a PUT on the membership
// invites an outsider and edits an insider.
func (w *GitHubWriter) SetMembership(ctx context.Context, org, login string, admin bool) error {
	role := "member"
	if admin {
		role = "admin"
	}

	_, _, err := w.client.Organizations.EditOrgMembership(ctx, login, org, &github.Membership{
		Role: github.Ptr(role),
	})
	if err != nil {
		return fmt.Errorf("set membership of %s in %s: %w", login, org, err)
	}

	return nil
}

// RemoveMember removes a login from the organization.
func (w *GitHubWriter) RemoveMember(ctx context.Context, org, login string) error {
	if _, err := w.client.Organizations.RemoveMember(ctx, org, login); err != nil {
		return fmt.Errorf("remove %s from %s: %w", login, org, err)
	}

	return nil
}

// CancelInvite withdraws a pending invitation.
func (w *GitHubWriter) CancelInvite(ctx context.Context, org string, invitationID int64) error {
	if _, err := w.client.Organizations.CancelInvite(ctx, org, invitationID); err != nil {
		return fmt.Errorf("cancel invitation %d in %s: %w", invitationID, org, err)
	}

	return nil
}

// AddTeamMember adds a login to an existing team, as a plain member.
func (w *GitHubWriter) AddTeamMember(ctx context.Context, org, slug, login string) error {
	_, _, err := w.client.Teams.AddTeamMembershipBySlug(ctx, org, slug, login,
		&github.TeamAddTeamMembershipOptions{Role: "member"})
	if err != nil {
		return fmt.Errorf("add %s to team %s in %s: %w", login, slug, org, err)
	}

	return nil
}

// RemoveTeamMember removes a login from a team.
func (w *GitHubWriter) RemoveTeamMember(ctx context.Context, org, slug, login string) error {
	if _, err := w.client.Teams.RemoveTeamMembershipBySlug(ctx, org, slug, login); err != nil {
		return fmt.Errorf("remove %s from team %s in %s: %w", login, slug, org, err)
	}

	return nil
}
