// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/lestrrat-go/jwx/v4/jwk"
)

// AppClient makes App-level (not installation-level) GitHub calls, authenticated
// with the App JWT. It needs only the App id and private key — no installation
// id — so it can be used before the installation is known.
type AppClient struct {
	appID  int64
	key    jwk.Key
	client *http.Client
}

// NewAppClient builds an App-level client from the App id and PEM private key.
func NewAppClient(appID int64, privateKey []byte, httpClient *http.Client) (*AppClient, error) {
	key, err := parseKey(privateKey)
	if err != nil {
		return nil, err
	}

	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}

	return &AppClient{appID: appID, key: key, client: httpClient}, nil
}

// FindInstallation returns the installation id of this App on the given
// organization. It authenticates with the App JWT (no installation id needed),
// so it works right after a manifest-created App is installed, to capture the
// id the token minting then requires. Not-installed is a distinct, actionable
// error. Roster's Apps are per-org (one installation), so a single page covers
// them; the limit is stated rather than silently truncating.
func (t *AppClient) FindInstallation(ctx context.Context, org string) (int64, error) {
	assertion, err := signAppJWT(t.appID, t.key)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/app/installations?per_page=100", http.NoBody)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("list installations: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("list installations: unexpected status %s", resp.Status)
	}

	var body []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode installations: %w", err)
	}

	for i := range body {
		if strings.EqualFold(body[i].Account.Login, org) {
			return body[i].ID, nil
		}
	}

	return 0, fmt.Errorf("app is not installed on organization %q", org)
}
