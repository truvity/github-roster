// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package githubapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/truvity/github-roster/pkg/githubapp"
)

func TestNewManifest(t *testing.T) {
	m := githubapp.NewManifest("roster-acme", "https://roster.example", "https://roster.example/cb")

	if m.Name != "roster-acme" || m.URL != "https://roster.example" || m.RedirectURL != "https://roster.example/cb" {
		t.Fatalf("manifest identity = %+v", m)
	}
	if m.Public {
		t.Error("the App must be private")
	}
	if m.DefaultPermissions["members"] != "write" {
		t.Errorf("members permission = %q, want write", m.DefaultPermissions["members"])
	}
	if active, _ := m.HookAttributes["active"].(bool); active {
		t.Error("the webhook must be inactive (the loop polls)")
	}
}

func TestCreateURL(t *testing.T) {
	if got := githubapp.CreateURL("acme"); got != "https://github.com/organizations/acme/settings/apps/new" {
		t.Errorf("CreateURL = %q", got)
	}
}

func TestConvertManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/app-manifests/") {
			http.Error(w, "unexpected", http.StatusBadRequest)

			return
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42,"slug":"roster-acme","client_id":"cid","client_secret":"csec","webhook_secret":"wsec","pem":"-----BEGIN KEY-----\nx\n-----END KEY-----","html_url":"https://github.com/apps/roster-acme"}`))
	}))
	defer srv.Close()

	// Point the client at the test server by rewriting the host via a custom
	// transport (ConvertManifest builds an api.github.com URL).
	client := &http.Client{Transport: rewriteHost{to: srv.URL, base: srv.Client().Transport}}

	reg, err := githubapp.ConvertManifest(context.Background(), client, "the-code")
	if err != nil {
		t.Fatalf("ConvertManifest: %v", err)
	}

	if reg.ID != 42 || reg.Slug != "roster-acme" || reg.ClientID != "cid" || reg.WebhookSecret != "wsec" {
		t.Errorf("registration = %+v", reg)
	}
	if !strings.Contains(reg.PEM, "BEGIN KEY") {
		t.Errorf("pem not captured: %q", reg.PEM)
	}
}

func TestConvertManifestErrors(t *testing.T) {
	if _, err := githubapp.ConvertManifest(context.Background(), http.DefaultClient, ""); err == nil {
		t.Error("empty code should error")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	client := &http.Client{Transport: rewriteHost{to: srv.URL, base: srv.Client().Transport}}
	if _, err := githubapp.ConvertManifest(context.Background(), client, "bad"); err == nil {
		t.Error("a non-201 status should error")
	}
}

// rewriteHost sends every request to the test server instead of api.github.com.
type rewriteHost struct {
	to   string
	base http.RoundTripper
}

func (h rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := req.URL.Parse(h.to)
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host

	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(req)
}
