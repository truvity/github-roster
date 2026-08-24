// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"encoding/base64"
	"testing"
)

func TestAppStateRoundTrip(t *testing.T) {
	state, err := newAppState("acme")
	if err != nil {
		t.Fatalf("newAppState: %v", err)
	}

	org, ok := verifyAppState(state, state)
	if !ok || org != "acme" {
		t.Fatalf("round-trip = (%q,%v), want (acme,true)", org, ok)
	}

	// Two states for the same org differ (the nonce).
	other, _ := newAppState("acme")
	if other == state {
		t.Error("states must be unguessable/unique")
	}
}

func TestVerifyAppStateRejects(t *testing.T) {
	good, _ := newAppState("acme")

	cases := map[string][2]string{
		"empty both":   {"", ""},
		"empty cookie": {"", good},
		"mismatch":     {good, good + "x"},
		"not base64":   {"!!!", "!!!"},
		"no org sep":   {b64("no-separator-here"), b64("no-separator-here")},
	}

	for name, cp := range cases {
		if _, ok := verifyAppState(cp[0], cp[1]); ok {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
