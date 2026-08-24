// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"context"

	"connectrpc.com/connect"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	rosterv1 "github.com/truvity/github-roster/gen/roster/v1"
	"github.com/truvity/github-roster/gen/roster/v1/rosterv1connect"
	"github.com/truvity/github-roster/pkg/config"
)

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

	resp := &rosterv1.GetSettingsResponse{
		Sources:      protoSources(data.Sources),
		StoreSources: protoSources(data.StoreSources),
		Orgs:         make([]*rosterv1.Org, 0, len(data.Orgs)),
	}

	for i := range data.Orgs {
		o := &data.Orgs[i]

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

		resp.Orgs = append(resp.Orgs, &rosterv1.Org{
			Name:             o.Name,
			Company:          o.Company,
			MinAdmins:        int32(o.MinAdmins), //nolint:gosec // small operator-set bound
			ReconcileEnabled: o.ReconcileEnabled,
			Teams:            teams,
		})
	}

	return connect.NewResponse(resp), nil
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

// registerConnect mounts the ConnectRPC service on the fiber app. The generated
// handler is a net/http handler; fiber's adaptor bridges it. The service prefix
// is routed with a wildcard so every method — and the Connect, gRPC and
// gRPC-Web protocols the handler negotiates — is served from one registration.
func registerConnect(deps *Deps, app *fiber.App) {
	path, handler := rosterv1connect.NewRosterServiceHandler(&rosterConnect{deps: deps})
	app.All(path+"*", adaptor.HTTPHandler(handler))
}
