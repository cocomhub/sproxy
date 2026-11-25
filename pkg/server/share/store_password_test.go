// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package share

import "testing"

// TestHashSharePassword 锁口令哈希：空口令=空串、非空为自描述串、同口令不同盐、可校验、错口令拒绝。
func TestHashSharePassword(t *testing.T) {
	t.Parallel()
	if h, err := HashSharePassword("  "); err != nil || h != "" {
		t.Fatalf("空口令应返回空串: %q %v", h, err)
	}
	h1, err := HashSharePassword("correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	h2, err := HashSharePassword("correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h1 == h2 {
		t.Fatal("同一口令两次哈希必须不同（随机盐）")
	}
	for _, h := range []string{h1, h2} {
		if !VerifySharePassword(h, "correct horse") {
			t.Fatalf("正确口令应通过: %s", h)
		}
		if VerifySharePassword(h, "wrong") {
			t.Fatal("错误口令必须拒绝")
		}
	}
	// 无口令保护的链接：任何输入都通过（不当作错误口令）。
	if !VerifySharePassword("", "anything") {
		t.Fatal("空哈希串（无口令）应直接通过")
	}
	// 损坏哈希串：必须拒绝（不得 fail-open）。
	for _, bad := range []string{"plain", "scrypt$1$2$3", "scrypt$x$8$1$AAAA$AAAA", "bcrypt$1$2$3$4$5"} {
		if VerifySharePassword(bad, "anything") {
			t.Fatalf("损坏哈希串不得通过: %q", bad)
		}
	}
}

// TestPasswordFingerprint 指纹不可逆、与哈希串一一对应、空串为空。
func TestPasswordFingerprint(t *testing.T) {
	t.Parallel()
	if passwordFingerprint("") != "" {
		t.Fatal("空哈希串指纹应为空")
	}
	h, _ := HashSharePassword("pw")
	fp := passwordFingerprint(h)
	if fp == "" || len(fp) != 8 {
		t.Fatalf("指纹长度异常: %q", fp)
	}
	if fp == h {
		t.Fatal("指纹不得等于哈希串")
	}
}
