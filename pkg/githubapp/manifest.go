// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Manifest is a GitHub App creation manifest. Roster posts it (as a
// self-submitting form) to CreateURL(org); the Org Owner reviews and creates
// the App, and GitHub redirects back to RedirectURL with a one-time code that
// ConvertManifest exchanges for the credentials. See
// https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	RedirectURL        string            `json:"redirect_url"`
	HookAttributes     map[string]any    `json:"hook_attributes"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events,omitempty"`
}

// NewManifest builds the manifest for an org's roster App. baseURL is roster's
// external URL; redirectURL is where GitHub returns the code. The App is
// PRIVATE, carries only organization-members write (enough to reconcile team
// and org membership), and has NO webhook — the reconcile loop polls, so
// nothing needs to be delivered to us.
func NewManifest(name, baseURL, redirectURL string) Manifest {
	return Manifest{
		Name:               name,
		URL:                baseURL,
		RedirectURL:        redirectURL,
		HookAttributes:     map[string]any{"active": false},
		Public:             false,
		DefaultPermissions: map[string]string{"members": "write"},
	}
}

// CreateURL is where the Org Owner is sent to create the App from the manifest
// (the manifest travels in a posted form field, the state in the query).
func CreateURL(org string) string {
	return "https://github.com/organizations/" + org + "/settings/apps/new"
}

// AppRegistration is the credential set GitHub returns when a manifest code is
// converted. InstallationID is deliberately absent: it exists only once the
// App is installed on the organization, captured separately.
type AppRegistration struct {
	ID            int64
	Slug          string
	ClientID      string
	ClientSecret  string
	WebhookSecret string
	PEM           string
	HTMLURL       string
}

// ConvertManifest exchanges the one-time code GitHub returns after App
// creation for the App's credentials (POST /app-manifests/{code}/conversions).
// The request is unauthenticated — the code IS the secret — and single-use.
func ConvertManifest(ctx context.Context, client *http.Client, code string) (*AppRegistration, error) {
	if code == "" {
		return nil, fmt.Errorf("manifest code is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.github.com/app-manifests/"+code+"/conversions", http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("convert manifest: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("convert manifest: unexpected status %s", resp.Status)
	}

	var body struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
		HTMLURL       string `json:"html_url"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode conversion: %w", err)
	}

	if body.ID == 0 || body.PEM == "" {
		return nil, fmt.Errorf("conversion returned no app id or private key")
	}

	return &AppRegistration{
		ID:            body.ID,
		Slug:          body.Slug,
		ClientID:      body.ClientID,
		ClientSecret:  body.ClientSecret,
		WebhookSecret: body.WebhookSecret,
		PEM:           body.PEM,
		HTMLURL:       body.HTMLURL,
	}, nil
}
