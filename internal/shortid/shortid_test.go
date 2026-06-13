// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shortid

import "testing"

func TestShortHash_Long(t *testing.T) {
	in := "abcdef0123456789"   // len 16
	want := "abcdef012345"     // first 12 chars
	if got := ShortHash(in); got != want {
		t.Errorf("ShortHash(%q) = %q, want %q", in, got, want)
	}
}

func TestShortHash_Short(t *testing.T) {
	in := "abc" // len 3
	if got := ShortHash(in); got != in {
		t.Errorf("ShortHash(%q) = %q, want %q", in, got, in)
	}
}

func TestShortHash_Exactly12(t *testing.T) {
	in := "abcdef012345" // len 12
	if got := ShortHash(in); got != in {
		t.Errorf("ShortHash(%q) = %q, want %q", in, got, in)
	}
}

func TestShortHash_Empty(t *testing.T) {
	in := ""
	if got := ShortHash(in); got != in {
		t.Errorf("ShortHash(%q) = %q, want %q", in, got, in)
	}
}

func TestShortHash_Boundary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"len13", "abcdef0123456", "abcdef012345"},
		{"len12", "abcdef012345", "abcdef012345"},
		{"len11", "abcdef01234", "abcdef01234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShortHash(tt.in); got != tt.want {
				t.Errorf("ShortHash(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
