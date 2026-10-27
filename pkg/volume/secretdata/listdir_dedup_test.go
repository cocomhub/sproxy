// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"

	storagepkg "github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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
	if err := fs.WriteFile(ctx, storagepkg.LogicalKey{}, "1/2/f.bin", bytes.NewReader(bytes.Repeat([]byte("x"), 64)), 64, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := count("1")
	if got["2"] != 1 {
		t.Fatalf("list(1) 中目录 2 出现 %d 次（应 1 次）", got["2"])
	}
}

// TestListDir_EntriesCarrySHA256 —— **外部卷列举带整文件 SHA-256**（S2，2026-10-27）。
//
// 缺陷背景：Web UI 在 baidupcs / secretdata 卷上删除/改名报「缺少 checksum」——根因之一
// 是 ExternalList 填充的 FileInfo.Checksum 恒空（A 类内容派生值的产出缺口）。secretdata 的
// 整文件哈希就在内存加密 meta（Original.SHA256），列举填它 O(1) 零额外 I/O，这是唯一能让
// 列表直接带 checksum 的卷类型（baidupcs 每文件 1 次远端 sidecar 读 ⇒ 保持 unknown）。
//
// 断言：ListDir 文件条目 Checksum == 写后 Stat 的 Checksum（同一 meta 来源）且非空、
// ChecksumType 自述 sha256；目录条目不携带 checksum。
func TestListDir_EntriesCarrySHA256(t *testing.T) {
	t.Parallel()
	fs := physEnv(t, Options{MetaPadBytes: 1 << 20})
	ctx := context.Background()
	content := bytes.Repeat([]byte("list-checksum"), 32) // 448B
	if err := fs.WriteFile(ctx, storagepkg.LogicalKey{}, "dir/a.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.MakeDir(ctx, "dir/empty"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	entries, err := fs.ListDir(ctx, "dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	var aBin *syncpkg.Entry
	for i := range entries {
		if entries[i].Name == "a.bin" {
			aBin = &entries[i]
		}
		if entries[i].Name == "empty" && entries[i].IsDir && entries[i].Checksum != "" {
			t.Fatalf("目录条目不应携带 checksum（目录无哈希）: %+v", entries[i])
		}
	}
	if aBin == nil {
		t.Fatalf("ListDir 缺 a.bin，entries=%+v", entries)
	}
	if aBin.Checksum == "" {
		t.Fatalf("文件条目 a.bin 缺 SHA-256（S2 产出契约未生效）: %+v", *aBin)
	}
	if aBin.ChecksumType != "sha256" {
		t.Fatalf("ChecksumType=%q，应自述 sha256（供上层按算法取用，禁止把非 SHA-256 当它）", aBin.ChecksumType)
	}

	// 与 Stat 同源：Stat 返回同一 meta 的 SHA-256（写后读回一致）。
	st, serr := fs.Stat(ctx, "dir/a.bin")
	if serr != nil || st == nil {
		t.Fatalf("Stat: %v", serr)
	}
	if st.Checksum != aBin.Checksum {
		t.Fatalf("ListDir.Checksum=%q 与 Stat.Checksum=%q 不一致（须同一内存 meta 来源）", aBin.Checksum, st.Checksum)
	}
}
