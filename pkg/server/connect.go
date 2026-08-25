// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	rosterv1 "github.com/truvity/github-roster/gen/roster/v1"
	"github.com/truvity/github-roster/gen/roster/v1/rosterv1connect"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/config"
)

// rfc3339 formats a timestamp for the wire (empty for the zero time), matching
// what the JSON API served.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.Format(time.RFC3339)
}

// rosterConnect implements the typed RosterService served over ConnectRPC,
// beside the huma JSON API. The SPA (Connect-Web) and Go clients share this one
// contract, so a drifted field is a compile error. Endpoints migrate off the
// /api/* JSON handlers one at a time; GetVersion is the first.
type rosterConnect struct {
	deps *Deps
}

// GetVersion returns the running build's identity (previously GET /api/version).
func (s *rosterConnect) GetVersion(
	_ context.Context,
	_ *connect.Request[rosterv1.GetVersionRequest],
) (*connect.Response[rosterv1.GetVersionResponse], error) {
	return connect.NewResponse(&rosterv1.GetVersionResponse{
		Version: s.deps.Version.Version,
		Commit:  s.deps.Version.Commit,
	}), nil
}

// GetSettings returns the directories, orgs and teams the Settings view shows
// (previously GET /api/settings), from the same buildSettings the SSR page uses.
func (s *rosterConnect) GetSettings(
	ctx context.Context,
	_ *connect.Request[rosterv1.GetSettingsRequest],
) (*connect.Response[rosterv1.GetSettingsResponse], error) {
	data := s.deps.buildSettings(ctx)

	return connect.NewResponse(&rosterv1.GetSettingsResponse{
		Sources:      protoSources(data.Sources),
		StoreSources: protoSources(data.StoreSources),
		Orgs:         protoOrgs(data.Orgs),
		StoreOrgs:    protoOrgs(data.StoreOrgs),
	}), nil
}

// protoOrgs maps settings orgs (git or store) to their proto form.
func protoOrgs(orgs []settingsOrg) []*rosterv1.Org {
	out := make([]*rosterv1.Org, 0, len(orgs))

	for i := range orgs {
		o := &orgs[i]

		teams := make([]*rosterv1.Team, 0, len(o.Teams))
		for j := range o.Teams {
			t := &o.Teams[j]
			teams = append(teams, &rosterv1.Team{
				Name:    t.Name,
				Groups:  t.Groups,
				Members: t.Members,
				Pinned:  t.Pinned,
			})
		}

		out = append(out, &rosterv1.Org{
			Name:             o.Name,
			Company:          o.Company,
			MinAdmins:        int32(o.MinAdmins), //nolint:gosec // small operator-set bound
			ReconcileEnabled: o.ReconcileEnabled,
			Provenance:       o.Provenance,
			Teams:            teams,
		})
	}

	return out
}

// protoSources maps directory sources to their proto form (the fields the
// Settings view reads — the SSM prefix for git credentials is deliberately
// omitted).
func protoSources(srcs []config.Source) []*rosterv1.DirectorySource {
	out := make([]*rosterv1.DirectorySource, 0, len(srcs))
	for i := range srcs {
		s := &srcs[i]
		out = append(out, &rosterv1.DirectorySource{
			Name:       s.Name,
			Domains:    s.Domains,
			Endpoint:   s.Endpoint,
			ProbeGroup: s.ProbeGroup,
		})
	}

	return out
}

// GetRoster returns the joined roster for the People view. The JSON
// /api/roster stays (a cross-repo contract for the gitops puller); this serves
// the SPA the same data, typed.
func (s *rosterConnect) GetRoster(
	ctx context.Context,
	_ *connect.Request[rosterv1.GetRosterRequest],
) (*connect.Response[rosterv1.GetRosterResponse], error) {
	joined, err := s.deps.buildRoster(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	people := make([]*rosterv1.Person, 0, len(joined.People))
	for i := range joined.People {
		p := &joined.People[i]

		orgs := make(map[string]*rosterv1.Membership, len(p.Orgs))
		for name, m := range p.Orgs {
			orgs[name] = &rosterv1.Membership{
				Member:            m.Member,
				InvitationPending: m.InvitationPending,
				Role:              string(m.Role),
			}
		}

		people = append(people, &rosterv1.Person{
			Name:   p.Name,
			Github: p.GitHub,
			Class:  string(p.Class),
			Live:   p.Live,
			State:  string(p.State),
			Orgs:   orgs,
		})
	}

	return connect.NewResponse(&rosterv1.GetRosterResponse{People: people}), nil
}

// GetStatus returns per-org reconcile status (Status view). The caller's bearer
// token is forwarded to the broker, which authorizes the human.
func (s *rosterConnect) GetStatus(
	ctx context.Context,
	req *connect.Request[rosterv1.GetStatusRequest],
) (*connect.Response[rosterv1.GetStatusResponse], error) {
	if s.deps.Broker == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no applier broker is configured"))
	}

	statuses, err := s.deps.Broker.ReconcileStatus(ctx, auth.ForwardToken(req.Header().Get))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}

	out := make([]*rosterv1.ReconcileStatus, 0, len(statuses))
	for i := range statuses {
		st := &statuses[i]
		out = append(out, &rosterv1.ReconcileStatus{
			Org:     st.Org,
			Enabled: st.Enabled,
			Paused:  st.Paused,
			At:      rfc3339(st.At),
			Actions: int32(st.Actions), //nolint:gosec // small action count
			Applied: st.Applied,
			Held:    st.Held,
			Reason:  st.Reason,
			Error:   st.Error,
		})
	}

	return connect.NewResponse(&rosterv1.GetStatusResponse{Statuses: out}), nil
}

// GetAudit returns audit records newest-first (History view).
func (s *rosterConnect) GetAudit(
	ctx context.Context,
	req *connect.Request[rosterv1.GetAuditRequest],
) (*connect.Response[rosterv1.GetAuditResponse], error) {
	if s.deps.Audit == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no audit sink is configured"))
	}

	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 50
	}

	records, err := s.deps.Audit.List(ctx, req.Msg.GetOrg(), limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	out := make([]*rosterv1.AuditRecord, 0, len(records))
	for i := range records {
		r := &records[i]

		changes := make([]*rosterv1.AuditChange, 0, len(r.Changes))
		for j := range r.Changes {
			c := &r.Changes[j]
			changes = append(changes, &rosterv1.AuditChange{
				Verb:    c.Verb,
				Subject: c.Subject,
				Team:    c.Team,
				Detail:  c.Detail,
			})
		}

		out = append(out, &rosterv1.AuditRecord{
			At:         rfc3339(r.At),
			Org:        r.Org,
			Kind:       string(r.Kind),
			Confirmed:  r.Confirmed,
			Actor:      r.Actor,
			ActorEmail: r.ActorEmail,
			Adding:     r.Adding,
			Removing:   r.Removing,
			Changes:    changes,
			Error:      r.Error,
		})
	}

	return connect.NewResponse(&rosterv1.GetAuditResponse{Records: out}), nil
}

// GetMe returns the signed-in caller as the gateway forwards it: name and
// email from the oauth2-proxy X-Auth-Request-* headers, role derived from the
// forwarded groups with the same operator-wins rule as token verification.
// When auth is disabled (local dev) every caller is the local operator.
func (s *rosterConnect) GetMe(
	ctx context.Context,
	req *connect.Request[rosterv1.GetMeRequest],
) (*connect.Response[rosterv1.GetMeResponse], error) {
	// The auth middleware already resolved the caller to a full Identity —
	// display name and email from the token (with userinfo fallback), role
	// from the groups claim — and exposed it on the request context. Prefer
	// it: the X-Auth-Request-User header is the provider's opaque subject id,
	// not a name.
	if id, ok := auth.FromContext(ctx); ok {
		return connect.NewResponse(&rosterv1.GetMeResponse{
			Name:  id.Name,
			Email: id.Email,
			Role:  string(id.Role),
		}), nil
	}

	// Fallback (identity not on the context): derive role from the forwarded
	// groups header. Email is real; the user header is the subject id.
	h := req.Header()
	role := auth.RoleFromGroups(auth.SplitGroups(h.Get("X-Auth-Request-Groups")), s.deps.Config.OIDC.Roles)

	return connect.NewResponse(&rosterv1.GetMeResponse{
		Name:  h.Get("X-Auth-Request-User"),
		Email: h.Get("X-Auth-Request-Email"),
		Role:  string(role),
	}), nil
}

// StageOrg stages an operator-added organization in the config store. It is
// operator-gated in-handler (the auth middleware put the identity on the
// context) — ConnectRPC POSTs carry a custom protocol header, so a browser
// cannot forge one cross-site, which is why no same-origin form guard is
// needed here as it was for the retired SSR form.
func (s *rosterConnect) StageOrg(
	ctx context.Context,
	req *connect.Request[rosterv1.StageOrgRequest],
) (*connect.Response[rosterv1.StageOrgResponse], error) {
	if id, ok := auth.FromContext(ctx); !ok || !id.Role.CanOperate() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("operator role required"))
	}

	if s.deps.OrgStore == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no config store is configured"))
	}

	m := req.Msg
	name := strings.TrimSpace(m.GetName())
	team := strings.TrimSpace(m.GetTeam())
	groups := trimmedNonEmpty(m.GetGroups())
	members := trimmedNonEmpty(m.GetMembers())

	if name == "" || team == "" || (len(groups) == 0 && len(members) == 0) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("a name, a team, and at least one group or member are required"))
	}

	org := config.Org{
		Name:      name,
		MinAdmins: int(m.GetMinAdmins()),
		Teams:     map[string]config.Team{team: {Groups: groups, Members: members}},
	}

	if err := s.deps.OrgStore.PutOrg(ctx, org); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&rosterv1.StageOrgResponse{}), nil
}

// trimmedNonEmpty trims each value and drops the empties.
func trimmedNonEmpty(in []string) []string {
	var out []string

	for _, v := range in {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}

	return out
}

// registerConnect mounts the ConnectRPC service on the fiber app. The generated
// handler is a net/http handler; fiber's adaptor bridges it. The service prefix
// is routed with a wildcard so every method — and the Connect, gRPC and
// gRPC-Web protocols the handler negotiates — is served from one registration.
func registerConnect(deps *Deps, app *fiber.App) {
	path, handler := rosterv1connect.NewRosterServiceHandler(&rosterConnect{deps: deps})
	app.All(path+"*", adaptor.HTTPHandler(handler))
}
