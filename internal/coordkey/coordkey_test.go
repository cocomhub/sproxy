// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordkey

import "testing"

// TestSanitize 覆盖 key 归一矩阵：路径穿越形状必须被归一（不产生越界路径），
// 合法 IP 原样保留，空 key 落安全名。
func TestSanitize(t *testing.T) {
	t.Parallel()
	// 路径穿越形状必须被归一（不产生越界路径）。
	if got := Sanitize("../etc/passwd"); got != "_._etc_passwd" {
		t.Fatalf("sanitize ../etc/passwd = %q, want _._etc_passwd", got)
	}
	if got := Sanitize("192.168.1.1"); got != "192.168.1.1" {
		t.Fatalf("sanitize ip = %q, want 192.168.1.1", got)
	}
	// 空 key 落安全名。
	if got := Sanitize(""); got != "_" {
		t.Fatalf("sanitize empty = %q, want _", got)
	}
}
