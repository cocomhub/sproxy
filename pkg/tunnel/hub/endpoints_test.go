// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import "testing"

func TestNormalizeEndpoints(t *testing.T) {
	cases := []struct {
		name      string
		hub       string
		server    string
		wantHTTP  string
		wantWS    string
		wantError bool
	}{
		{"empty both", "", "", "", "", true},
		{"empty falls back to server", "", "https://s:18083", "https://s:18083", "wss://s:18083/ws", false},
		{"http", "http://h:18083", "", "http://h:18083", "ws://h:18083/ws", false},
		{"https", "https://h:18083/x", "", "https://h:18083", "wss://h:18083/ws", false},
		{"ws", "ws://h:18084/ws", "", "http://h:18084", "ws://h:18084/ws", false},
		{"wss", "wss://h:18084/ws", "", "https://h:18084", "wss://h:18084/ws", false},
		{"unknown scheme", "ftp://h", "", "", "", true},
		{"malformed", "not a url", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpBase, wsURL, err := NormalizeEndpoints(tc.hub, tc.server)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got http=%q ws=%q", httpBase, wsURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if httpBase != tc.wantHTTP || wsURL != tc.wantWS {
				t.Fatalf("got http=%q ws=%q, want http=%q ws=%q", httpBase, wsURL, tc.wantHTTP, tc.wantWS)
			}
		})
	}
}
