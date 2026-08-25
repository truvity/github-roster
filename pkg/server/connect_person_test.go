// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rosterv1 "github.com/truvity/github-roster/gen/roster/v1"
	"github.com/truvity/github-roster/pkg/auth"
	"github.com/truvity/github-roster/pkg/mapping"
)

func TestPutPersonStampsApprovalOnFirstBless(t *testing.T) {
	store := mapping.NewMemory()
	s := &rosterConnect{deps: &Deps{Mapping: store}}

	ctx := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleOperator, Email: "op@truvity.com"})

	_, err := s.PutPerson(ctx, connect.NewRequest(&rosterv1.PutPersonRequest{
		Name: "Ada Lovelace", Github: "ada", K8S: "ada", Class: "employee",
	}))
	if err != nil {
		t.Fatalf("PutPerson: %v", err)
	}

	got, err := store.Get(context.Background(), "Ada Lovelace")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ApprovedBy != "op@truvity.com" {
		t.Errorf("approvedBy = %q, want op@truvity.com", got.ApprovedBy)
	}

	if got.ApprovedAt == "" {
		t.Error("approvedAt not stamped")
	}
}

func TestPutPersonPreservesApprovalOnEdit(t *testing.T) {
	store := mapping.NewMemory()
	s := &rosterConnect{deps: &Deps{Mapping: store}}

	first := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleOperator, Email: "first@truvity.com"})
	_, err := s.PutPerson(first, connect.NewRequest(&rosterv1.PutPersonRequest{Name: "Ada", Github: "ada", K8S: "ada", Class: "employee"}))
	if err != nil {
		t.Fatalf("first PutPerson: %v", err)
	}

	stamped, _ := store.Get(context.Background(), "Ada")

	// A later edit by a different operator keeps the original approver.
	second := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleOperator, Email: "second@truvity.com"})
	_, err = s.PutPerson(second, connect.NewRequest(&rosterv1.PutPersonRequest{Name: "Ada", Github: "ada-l", K8S: "ada", Class: "employee"}))
	if err != nil {
		t.Fatalf("edit PutPerson: %v", err)
	}

	got, _ := store.Get(context.Background(), "Ada")
	if got.ApprovedBy != "first@truvity.com" || got.ApprovedAt != stamped.ApprovedAt {
		t.Errorf("edit changed approval: by=%q at=%q, want first@truvity.com / %q", got.ApprovedBy, got.ApprovedAt, stamped.ApprovedAt)
	}

	if got.GitHub != "ada-l" {
		t.Errorf("edit did not apply: github = %q", got.GitHub)
	}
}

func TestPutPersonRejectsViewer(t *testing.T) {
	s := &rosterConnect{deps: &Deps{Mapping: mapping.NewMemory()}}

	ctx := auth.WithIdentity(context.Background(), auth.Identity{Role: auth.RoleViewer})

	_, err := s.PutPerson(ctx, connect.NewRequest(&rosterv1.PutPersonRequest{Name: "Ada", Github: "ada"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer PutPerson code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}
