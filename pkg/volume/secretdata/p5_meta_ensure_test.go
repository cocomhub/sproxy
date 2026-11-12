// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// p5_meta_ensure_test.go 钉住 P5（2026-11-12）修复的核心不变量：
//
// P1（#1117）之后 `metaEntry.meta` 是**常态 nil**（快照不承载冷结构 + L1b 按需加载/淘汰），
// 因此任何「真的需要 meta」的路径都必须**先确保已加载**，而不能把「未加载」当成
// 「文件不存在 / 元信息缺失」——
//   - 修前：`FileMeta`/`BlockletMode` 直接报「元信息缺失（不可校验）」= **功能失效**；
//     `UpdateMetaExtra` 在持锁时判 `e.meta == nil` ⇒ 把**存在的文件**报成「不存在」。
//
// 变异：删掉任一处 `ensureEntryMeta` 调用 ⇒ 对应用例红。

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newP5FS 建一个最小可用的加密卷并写入一个文件，然后**人为清空其 meta + L1b**
// （精确模拟 P1 之后的真实状态：快照加载完成 / L1b 淘汰）。
func newP5FS(t *testing.T, rel string) *SecretdataFS {
	t.Helper()
	inner := mkLocalFS(t, "backing")
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	content := bytes.Repeat([]byte("p5 on-demand meta "), 64)
	if werr := fs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 模拟 P1 常态：条目只留热字段，meta 未加载且不在 L1b 中。
	fs.mu.Lock()
	e := fs.index[rel]
	if e == nil {
		fs.mu.Unlock()
		t.Fatalf("索引里应有 %q", rel)
	}
	e.meta = nil
	fs.mu.Unlock()
	fs.metaCache.clear()
	return fs
}

// TestP5_FileMetaLoadsOnDemand 修前：`FileMeta` 报「元信息缺失（不可校验）」。
func TestP5_FileMetaLoadsOnDemand(t *testing.T) {
	t.Parallel()
	const rel = "d/f.bin"
	fs := newP5FS(t, rel)
	fm, err := fs.FileMeta(context.Background(), rel)
	if err != nil {
		t.Fatalf("meta 未加载时应自动按需加载，而不是报错: %v", err)
	}
	if fm == nil || fm.Name == "" || fm.Size == 0 {
		t.Fatalf("应返回有效 FileMeta，得到 %+v", fm)
	}
}

// TestP5_BlockletModeLoadsOnDemand 修前：`BlockletMode` 报「元信息缺失」。
func TestP5_BlockletModeLoadsOnDemand(t *testing.T) {
	t.Parallel()
	const rel = "d/b.bin"
	fs := newP5FS(t, rel)
	if _, err := fs.BlockletMode(context.Background(), rel); err != nil {
		t.Fatalf("meta 未加载时应自动按需加载，而不是报错: %v", err)
	}
}

// TestP5_UpdateMetaExtraLoadsOnDemand 修前：持锁判 `e.meta == nil` ⇒ 把存在的文件
// 报成「文件不存在」。
func TestP5_UpdateMetaExtraLoadsOnDemand(t *testing.T) {
	t.Parallel()
	const rel = "d/u.bin"
	fs := newP5FS(t, rel)
	ctx := context.Background()
	if err := fs.UpdateMetaExtra(ctx, rel, map[string]any{"source_integrity": "damaged"}); err != nil {
		t.Fatalf("meta 未加载时应自动按需加载后更新，而不是报「不存在」: %v", err)
	}
	// 回读确认真的写进去了（避免「没报错但没生效」的假绿）。
	fm, err := fs.FileMeta(ctx, rel)
	if err != nil {
		t.Fatalf("FileMeta: %v", err)
	}
	if got, ok := fm.Extra["source_integrity"]; !ok || got != "damaged" {
		t.Fatalf("Extra 应含 source_integrity=damaged，得到 %v", fm.Extra)
	}
}

// TestP5_TraceLoadsMetaOnDemand 审查（trace）需要真实落点 ⇒ 修前会把「未加载」误报成
// `AnomalyMetaUnreadable`（假异常）。
func TestP5_TraceLoadsMetaOnDemand(t *testing.T) {
	t.Parallel()
	const rel = "d/t.bin"
	fs := newP5FS(t, rel)
	tr, err := fs.Trace(context.Background(), rel, syncpkg.InspectIndex)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	for _, a := range tr.Anomalies {
		if a.Code == syncpkg.AnomalyMetaUnreadable {
			t.Fatalf("审查不应把「按需加载前」误报为 meta 不可读: %+v", a)
		}
	}
}
