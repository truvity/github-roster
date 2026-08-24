// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/truvity/github-roster/pkg/configstore"
	"github.com/truvity/github-roster/pkg/githubapp"
)

const appStateCookie = "roster_app_state"

// manifestForm is a self-submitting form that POSTs the App manifest to GitHub.
// The manifest travels in a form field; the state in the query. It submits on
// load so the operator lands straight on GitHub's App-creation review screen.
var manifestForm = template.Must(template.New("manifest").Parse(
	`<!doctype html><html><body onload="document.forms[0].submit()">
<form action="{{.Action}}" method="post">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<noscript><button type="submit">Continue to GitHub</button></noscript>
</form></body></html>`))

// newAppState returns an unguessable state that also binds the target org:
// base64url("<org>\x1f<16 random bytes>"). The same value is set as a cookie
// AND sent to GitHub; the callback requires them to match (double-submit),
// which is the CSRF defense for the manifest round-trip — stateless, so it
// works across replicas without shared server state.
func newAppState(org string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(append([]byte(org+"\x1f"), nonce...)), nil
}

// verifyAppState requires the cookie and the returned state to be equal and
// non-empty, and returns the org they bind.
func verifyAppState(cookie, param string) (org string, ok bool) {
	if cookie == "" || cookie != param {
		return "", false
	}

	raw, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil {
		return "", false
	}

	sep := strings.IndexByte(string(raw), '\x1f')
	if sep <= 0 {
		return "", false
	}

	return string(raw[:sep]), true
}

// handleCreateApp starts the GitHub App-manifest flow for an org: it sets the
// state cookie and hands the operator a self-submitting form to GitHub, where
// the Org Owner reviews and creates the App. Operator-gated + same-origin.
func (d *Deps) handleCreateApp(c fiber.Ctx) error {
	org := strings.TrimSpace(c.Query("org"))
	if org == "" {
		return fiber.NewError(fiber.StatusBadRequest, "org is required")
	}

	state, err := newAppState(org)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not start the App flow")
	}

	c.Cookie(&fiber.Cookie{
		Name: appStateCookie, Value: state, Path: "/",
		HTTPOnly: true, Secure: true, SameSite: "Lax", MaxAge: 600,
	})

	base := c.BaseURL()

	manifest, err := json.Marshal(githubapp.NewManifest("roster-"+org, base, base+"/settings/orgs/app-callback"))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not build the manifest")
	}

	c.Set("Content-Type", "text/html; charset=utf-8")

	return manifestForm.Execute(c.Response().BodyWriter(), struct {
		Action   string
		Manifest string
	}{
		Action:   githubapp.CreateURL(org) + "?state=" + url.QueryEscape(state),
		Manifest: string(manifest),
	})
}

// handleAppCallback is where GitHub redirects the browser after the App is
// created. It verifies the double-submit state, exchanges the one-time code for
// the credentials, and stores them under the org. This is a cross-site
// redirect by design, so it is NOT same-origin gated — the state cookie is the
// CSRF defense; it stays operator-gated.
func (d *Deps) handleAppCallback(c fiber.Ctx) error {
	code := strings.TrimSpace(c.Query("code"))

	org, ok := verifyAppState(c.Cookies(appStateCookie), strings.TrimSpace(c.Query("state")))

	// One-shot: clear the state cookie regardless of outcome.
	c.Cookie(&fiber.Cookie{Name: appStateCookie, Value: "", Path: "/", MaxAge: -1})

	if !ok || code == "" {
		return c.Redirect().To("/settings?flash=" + url.QueryEscape("App creation could not be verified — please retry"))
	}

	if d.OrgStore == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "no config store is configured")
	}

	reg, err := githubapp.ConvertManifest(c.Context(), http.DefaultClient, code)
	if err != nil {
		return c.Redirect().To("/settings?flash=" + url.QueryEscape("converting the App manifest failed: "+err.Error()))
	}

	if err := d.OrgStore.PutApp(c.Context(), org, configstore.AppCredentials{
		AppID:         reg.ID,
		PrivateKey:    reg.PEM,
		ClientID:      reg.ClientID,
		ClientSecret:  reg.ClientSecret,
		WebhookSecret: reg.WebhookSecret,
	}); err != nil {
		return c.Redirect().To("/settings?flash=" + url.QueryEscape("storing the App credentials failed: "+err.Error()))
	}

	install := reg.HTMLURL + "/installations/new"

	return c.Redirect().To("/settings?flash=" + url.QueryEscape(
		"created App '"+reg.Slug+"' for "+org+" — now install it on the organization: "+install))
}
