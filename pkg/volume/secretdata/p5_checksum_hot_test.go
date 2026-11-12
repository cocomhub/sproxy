// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// p5_checksum_hot_test.go 钉住 P5「字段级别路由」的核心保证：
//
// `ChecksumAndSize`（实现 `meta.ChecksumProvider`）必须**只用热字段**给出
// 「整文件 checksum + 大小」，**完全不加载冷结构 `meta`**。
//
// 为什么关键：实测单个大文件 `Meta` 常驻 **78 MB**、解密+解析 ≈ **584 ms**；
// 而外部卷 `ExternalStat`（**热路径**）等调用方只用 `TotalSHA256`（+ Size）。
//
// 变异：把实现改成 `return e.meta.Original.SHA256`（需加载）⇒ 本用例红。

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// TestChecksumAndSize_DoesNotLoadMeta 决定性判据：调用后 `e.meta` **仍为 nil**。
// 只断言"返回了非空 checksum"是不够的 —— 加载后返回也算"通过"，但那正是要避免的成本。
func TestChecksumAndSize_DoesNotLoadMeta(t *testing.T) {
	t.Parallel()
	const rel = "d/cs.bin"
	fs := newP5FS(t, rel) // 已把 e.meta 置 nil 且清空 L1b（模拟 P1 后的常态）

	fs.mu.RLock()
	if fs.index[rel] == nil {
		fs.mu.RUnlock()
		t.Fatalf("索引里应有 %q", rel)
	}
	hotBefore := fs.index[rel].hotOrigSHA256()
	fs.mu.RUnlock()

	ck, size, err := fs.ChecksumAndSize(context.Background(), rel)
	if err != nil {
		t.Fatalf("ChecksumAndSize: %v", err)
	}
	if size <= 0 {
		t.Fatalf("应返回文件大小，得到 %d", size)
	}
	// 热字段有值 ⇒ checksum 应有值（该测试用 WriteFile 写入，热字段已由写路径填充）。
	if ck == "" {
		t.Fatal("热字段存在时应能给出 checksum（不应为空）")
	}
	if ck != hotBefore {
		t.Fatalf("checksum 应取自热字段 %q，得到 %q", hotBefore, ck)
	}

	fs.mu.RLock()
	stillNil := fs.index[rel].meta == nil
	fs.mu.RUnlock()
	if !stillNil {
		t.Fatal("ChecksumAndSize **不得**加载冷结构 meta（加载即失去本优化的意义）")
	}
}

// TestChecksumAndSize_MissingFileErrors 文件不存在 ⇒ 明确报错（不静默返回空）。
func TestChecksumAndSize_MissingFileErrors(t *testing.T) {
	t.Parallel()
	fs := newP5FS(t, "d/real.bin")
	if _, _, err := fs.ChecksumAndSize(context.Background(), "d/nope.bin"); err == nil {
		t.Fatal("不存在的文件必须返回错误")
	}
}

// TestAssertChecksumProvider 锁定可选能力的可发现性（未实现者 ⇒ ok=false，调用方回落重路径）。
func TestAssertChecksumProvider(t *testing.T) {
	t.Parallel()
	fs := newP5FS(t, "d/app.bin")
	if _, ok := meta.AssertChecksumProvider(fs); !ok {
		t.Fatal("secretdata 必须实现 meta.ChecksumProvider")
	}
	if _, ok := meta.AssertChecksumProvider(struct{}{}); ok {
		t.Fatal("未实现者应返回 ok=false")
	}
}
