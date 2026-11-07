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

// TestP3_1_AlnumFromBytesGuardAndDistribution 钉住 **P3-1**：空输入/非正长度不 panic；
// 拒绝采样后各字符出现次数差不超过 1（无取模偏置）。
func TestP3_1_AlnumFromBytesGuardAndDistribution(t *testing.T) {
	t.Parallel()
	if got := alnumFromBytes(nil, 8); got != "" {
		t.Fatalf("空输入应返回空串，got %q", got)
	}
	if got := alnumFromBytes([]byte{1, 2, 3}, 0); got != "" {
		t.Fatalf("n=0 应返回空串，got %q", got)
	}
	if got := alnumFromBytes([]byte{1, 2, 3}, 16); len(got) != 16 {
		t.Fatalf("应返回 16 字符，got %d", len(got))
	}
	// 分布：把全部 256 字节重复喂入 ⇒ 字符计数极差应 ≤ 1（拒绝采样消除取模偏置）
	pool := make([]byte, 256)
	for i := range pool {
		pool[i] = byte(i)
	}
	counts := map[byte]int{}
	for _, c := range []byte(alnumFromBytes(pool, 1000)) {
		counts[c]++
	}
	mn, mx := 1<<30, 0
	for _, c := range []byte(dirAlnumCharset) {
		n := counts[c]
		if n < mn {
			mn = n
		}
		if n > mx {
			mx = n
		}
	}
	if mx-mn > 1 {
		t.Fatalf("拒绝采样后应近似均匀（极差 ≤1），got min=%d max=%d", mn, mx)
	}
}

// TestF6_AlnumFromBytesTerminatesOnDegenerateInput 钉住**修复轮 F6**：输入字节全落在被拒区间
// （退化/恶意输入）时必须有**尝试上限**并兜底返回，绝不因纯拒绝采样而死循环。
// （红态验证：去掉兜底循环后用 `go test -timeout 30s` 运行本用例 ⇒ 超时失败。）
func TestF6_AlnumFromBytesTerminatesOnDegenerateInput(t *testing.T) {
	t.Parallel()
	got := alnumFromBytes([]byte{255, 255, 255, 255}, 8)
	if len(got) != 8 {
		t.Fatalf("退化输入应兜底返回 8 字符（不得死循环），got %q", got)
	}
}
