// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package githubapp_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truvity/github-roster/pkg/githubapp"
)

func testKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestFindInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":7,"account":{"login":"acme"}},{"id":8,"account":{"login":"beta"}}]`))
	}))
	defer srv.Close()

	ts, err := githubapp.NewAppClient(123, testKeyPEM(t), &http.Client{Transport: rewriteHost{to: srv.URL}})
	if err != nil {
		t.Fatalf("NewAppClient: %v", err)
	}

	id, err := ts.FindInstallation(context.Background(), "acme")
	if err != nil || id != 7 {
		t.Fatalf("FindInstallation(acme) = (%d,%v), want (7,nil)", id, err)
	}

	// Case-insensitive on the org login.
	if id, err := ts.FindInstallation(context.Background(), "ACME"); err != nil || id != 7 {
		t.Errorf("FindInstallation(ACME) = (%d,%v), want (7,nil)", id, err)
	}

	// Not installed is a distinct error, not a silent zero.
	if _, err := ts.FindInstallation(context.Background(), "zeta"); err == nil {
		t.Error("FindInstallation on an uninstalled org should error")
	}
}
