// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"
)

// TestListDir_NoDuplicateDirAcrossSources 钉住**跨源去重**（用户实测 2026-10-14：secretdata 卷
// `list 1/` 输出两个 `2/`）。
//
// 根因：同一子目录同时由两个来源产出——`dirs`（目录键，dir meta 加载）与 `index`（文件键
// `1/2/f.bin` 的首段推出目录）⇒ 两个收集函数各自内部去重、**互不知道对方** ⇒ 简单 append 必重复。
// 本测同时覆盖两种状态：空目录（只有 `dirs` 来源）与有文件（两来源都命中）。
func TestListDir_NoDuplicateDirAcrossSources(t *testing.T) {
	t.Parallel()
	fs := physEnv(t, Options{MetaPadBytes: 1 << 20})
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "1/2"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	count := func(rel string) map[string]int {
		entries, err := fs.ListDir(ctx, rel)
		if err != nil {
			t.Fatalf("ListDir(%s): %v", rel, err)
		}
		m := map[string]int{}
		for _, e := range entries {
			m[e.Name]++
		}
		return m
	}
	if got := count("1"); got["2"] != 1 {
		t.Fatalf("空目录 list(1) 中 2 出现 %d 次（应 1 次）: %v", got["2"], got)
	}
	// 写入文件后（索引前缀也会贡献目录条目）
	if err := fs.WriteFile(ctx, "1/2/f.bin", bytes.NewReader(bytes.Repeat([]byte("x"), 64)), 64, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := count("1")
	if got["2"] != 1 {
		t.Fatalf("list(1) 中目录 2 出现 %d 次（应 1 次）", got["2"])
	}
}
