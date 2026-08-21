// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"testing"
)

func TestDialAllowed(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{"loopback", "127.0.0.1:22", true},
		{"private-10", "10.0.0.5:8080", true},
		{"private-192", "192.168.1.100:22", true},
		{"private-172", "172.16.3.9:443", true},
		{"public-ip", "1.2.3.4:80", false},
		{"hostname", "sg-vps-2.example.com:22", true},
		{"bad-no-port", "127.0.0.1", false},
		{"bad-garbage", ":::", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DialAllowed(tc.addr); got != tc.want {
				t.Fatalf("DialAllowed(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
