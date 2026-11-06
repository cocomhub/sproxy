// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import "testing"

// TestDirNameFor_Deterministic 钉住**确定性容器命名**（2026-11-06 修复重复容器的机制基础）：
// 同 (密钥, 父容器, 逻辑路径) 必须恒得同名 ⇒ 「重复创建」天然幂等 ⇒ 不会出现两个容器
// 声明同一逻辑目录（那会导致装配期 dirSegs 静默覆盖、卷拒绝挂载）。
func TestDirNameFor_Deterministic(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-key-000")
	a, err := DirNameFor(secret, "P1", "jav/makers/NHDTB")
	if err != nil {
		t.Fatalf("DirNameFor: %v", err)
	}
	b, err := DirNameFor(secret, "P1", "jav/makers/NHDTB")
	if err != nil {
		t.Fatalf("DirNameFor: %v", err)
	}
	if a != b {
		t.Fatalf("同参数必须同名（幂等），got %q vs %q", a, b)
	}
	if len(a) != dirNameLen {
		t.Fatalf("长度应为 %d，got %d", dirNameLen, len(a))
	}
	// 字符域与随机名一致（外观不可区分）
	for _, ch := range a {
		if !containsRune(dirAlnumCharset, ch) {
			t.Fatalf("字符 %q 不在容器名域内", ch)
		}
	}
	// 不同父 / 不同路径 / 不同密钥 ⇒ 必须不同名
	diff := []string{}
	d2, _ := DirNameFor(secret, "P2", "jav/makers/NHDTB")
	d3, _ := DirNameFor(secret, "P1", "jav/makers/NHDTA")
	d4, _ := DirNameFor([]byte("other-secret-key-01"), "P1", "jav/makers/NHDTB")
	for _, d := range []string{d2, d3, d4} {
		if d == a {
			diff = append(diff, d)
		}
	}
	if len(diff) > 0 {
		t.Fatalf("不同父/路径/密钥必须派生不同名，冲突: %v", diff)
	}
}

// TestDirNameFor_AnonymityAcrossVolumeKeys 跨卷不可关联：不同卷密钥派生不同名。
func TestDirNameFor_AnonymityAcrossVolumeKeys(t *testing.T) {
	t.Parallel()
	path := "a/b/c"
	x, err := DirNameFor([]byte("volume-key-A-0123456789"), "root", path)
	if err != nil {
		t.Fatalf("DirNameFor: %v", err)
	}
	y, err := DirNameFor([]byte("volume-key-B-0123456789"), "root", path)
	if err != nil {
		t.Fatalf("DirNameFor: %v", err)
	}
	if x == y {
		t.Fatalf("不同卷密钥不得派生出同名（跨卷可关联）: %q", x)
	}
}

// TestDirNameFor_EmptySecretFails 空密钥必须 fail-closed（否则退化为可枚举）。
func TestDirNameFor_EmptySecretFails(t *testing.T) {
	t.Parallel()
	if _, err := DirNameFor(nil, "p", "d"); err == nil {
		t.Fatal("空密钥必须报错（fail-closed）")
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
