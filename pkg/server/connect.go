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

// registerConnect mounts the ConnectRPC service on the fiber app. The generated
// handler is a net/http handler; fiber's adaptor bridges it. The service prefix
// is routed with a wildcard so every method — and the Connect, gRPC and
// gRPC-Web protocols the handler negotiates — is served from one registration.
func registerConnect(deps *Deps, app *fiber.App) {
	path, handler := rosterv1connect.NewRosterServiceHandler(&rosterConnect{deps: deps})
	app.All(path+"*", adaptor.HTTPHandler(handler))
}
