// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

import "testing"

// TestEqual_ConstantTimeComparison 钉住 checksum.Equal（审查 P2 收口）：
// 相等/不等/长度不同（快速短路）三态。
func TestEqual_ConstantTimeComparison(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"equal", "a1b2c3d4", "a1b2c3d4", true},
		{"case differs", "A1B2C3D4", "a1b2c3d4", false},
		{"diff char", "a1b2c3d4", "a1b2c3d5", false},
		{"len differs", "a1b2c3d4", "a1b2c3d4e", false},
		{"empty", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Equal(c.a, c.b); got != c.want {
				t.Fatalf("Equal(%q,%q)=%v want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
