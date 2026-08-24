// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rosterv1 "github.com/truvity/github-roster/gen/roster/v1"
	"github.com/truvity/github-roster/pkg/version"
)

func TestRosterConnectGetVersion(t *testing.T) {
	s := &rosterConnect{deps: &Deps{Version: version.Info{Version: "1.2.3", Commit: "abcdef0"}}}

	resp, err := s.GetVersion(context.Background(), connect.NewRequest(&rosterv1.GetVersionRequest{}))
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}

	if got := resp.Msg.GetVersion(); got != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", got)
	}

	if got := resp.Msg.GetCommit(); got != "abcdef0" {
		t.Errorf("commit = %q, want abcdef0", got)
	}
}
