// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// copy_phys_test.go 钉住「复制必须记录 phys」（独立审核 P1，2026-10-18）。
//
// `metaEntry.phys` 是**本条目在本卷的物理落盘字节**，而 `TrueUsage()`（`trueusage.go`）=
// Σ `e.phys`、`PhysFootprint` = `TrueSize` ⇒ 真值口径**直接吃该字段**；对账
// `correctLedgerToTruth` 又把它**当权威回写**账本与卷池（`quota_reconcile.go`）。
// `copy.go` 的 `s.index[t] = &metaEntry{...}` 是**唯一**不设 `phys` 的构造点 ⇒ 复制产物
// `phys==0` ⇒ 真值少计 ⇒ 对账把卷池**压到低于磁盘**（复制 4KiB 文件实际新增 1–2MiB meta
// blob，却只记 4KiB）。
//
// 语义（与 copy.go 顶部设计一致）：
//   - **同容器** ⇒ 分块**原地共享**（已由源条目计入）⇒ 副本只计新 meta blob（含 pad）；
//   - **跨容器** ⇒ 分块（+parity）已**字节拷贝** ⇒ 副本计 meta + 内容 + parity。

import (
	"bytes"
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestCopyFile_RecordsPhysForBothContainers 复制产物必须记录 phys，且同/跨容器口径不同。
func TestCopyFile_RecordsPhysForBothContainers(t *testing.T) {
	t.Parallel()
	fs, _ := newCountingFS(t)
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "a"); err != nil {
		t.Fatalf("MakeDir(a): %v", err)
	}
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir(b): %v", err)
	}
	data := bytes.Repeat([]byte("x"), 4096)
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/src.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	src := fs.index["a/src.bin"]
	if src == nil || src.phys <= 0 {
		t.Fatalf("前置：源条目 phys 必须 >0（%+v）", src)
	}

	// 同容器复制：a/src.bin → a/dst.bin
	if err := fs.CopyFile(ctx, "a/src.bin", "a/dst.bin"); err != nil {
		t.Fatalf("CopyFile(同容器): %v", err)
	}
	same := fs.index["a/dst.bin"]
	if same == nil {
		t.Fatal("同容器副本缺失")
	}
	if same.phys <= 0 {
		t.Fatalf("同容器副本 phys=%d 必须 >0（未修必红：漏设 ⇒ 0 ⇒ TrueUsage 少计）", same.phys)
	}
	// 内容共享 ⇒ 副本只计 meta（含 pad）⇒ 必然严格小于源（meta + 内容）。
	if same.phys >= src.phys {
		t.Fatalf("同容器副本 phys=%d 必须 < 源 phys=%d（内容共享、只新增 meta blob）", same.phys, src.phys)
	}

	// 跨容器复制：a/src.bin → b/dst.bin
	if err := fs.CopyFile(ctx, "a/src.bin", "b/dst.bin"); err != nil {
		t.Fatalf("CopyFile(跨容器): %v", err)
	}
	cross := fs.index["b/dst.bin"]
	if cross == nil {
		t.Fatal("跨容器副本缺失")
	}
	if cross.phys <= 0 {
		t.Fatalf("跨容器副本 phys=%d 必须 >0（未修必红）", cross.phys)
	}
	// 内容被物理拷贝 ⇒ 跨容器副本必须**大于**同容器副本（后者不含内容）。
	if cross.phys <= same.phys {
		t.Fatalf("跨容器副本 phys=%d 必须 > 同容器副本 phys=%d（内容已字节拷贝）", cross.phys, same.phys)
	}
}
