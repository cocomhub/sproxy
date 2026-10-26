// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestWriteMeta_OverwriteKeepsOldExtra 钉住用户裁定（2026-10-26：默认**新增而非覆盖**）：
// 本地卷覆盖写（二次上传）后，旧 sidecar 的 extra 键必须保留，本次新增键追加。
//
// 为什么这是 P0：`writeMetaCore` 的 `computeMeta` 从主文件**重算** FileMeta（Extra 为空）；
// 若直接 `applyMetaExtra(fm, extra)`，覆盖写会把旧 KV 整体丢失——与「默认新增」裁定相反。
// 变异：删掉 writeMetaCore 里读旧 sidecar 的那段 → 本用例红。
func TestWriteMeta_OverwriteKeepsOldExtra(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	p := filesMetaPolicy{h: &Handlers{}}
	ctx := context.Background()
	const rel = "user/a.txt"

	// 第一次写 + extra{code:OLD}
	writeMetaProbeFile(t, root, rel, "v1")
	if err := p.WriteMeta(ctx, "alice", root, rel, map[string]any{"code": "OLD"}); err != nil {
		t.Fatalf("WriteMeta#1: %v", err)
	}
	// 第二次覆盖写 + extra{maker:NEW}（应合并而非覆盖）
	writeMetaProbeFile(t, root, rel, "v2")
	if err := p.WriteMeta(ctx, "alice", root, rel, map[string]any{"maker": "NEW"}); err != nil {
		t.Fatalf("WriteMeta#2: %v", err)
	}

	raw, rerr := p.ReadMetaRaw(ctx, root, meta.MetaPath(rel))
	if rerr != nil {
		t.Fatalf("ReadMetaRaw: %v", rerr)
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("Unmarshal: %v", uerr)
	}
	if fm.Extra["code"] != "OLD" {
		t.Fatalf("覆盖写后旧键 code 丢失（合并语义未生效）：%v", fm.Extra)
	}
	if fm.Extra["maker"] != "NEW" {
		t.Fatalf("本次新增键 maker 未写入：%v", fm.Extra)
	}
	// 主文件哈希必须是新内容（不被旧 meta 污染）。
	if !strings.HasPrefix(fm.TotalSHA256, "") || fm.Size != 2 {
		t.Fatalf("meta 内容基线错（应为新内容 size=2）：size=%d", fm.Size)
	}
}

// writeMetaProbeFile 经 storage.Root 写文件（探针辅助：只测 meta 落盘语义）。
func writeMetaProbeFile(t *testing.T, root *storage.Root, rel, content string) {
	t.Helper()
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil && !os.IsExist(err) {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, werr := f.WriteString(content); werr != nil {
		_ = f.Close()
		t.Fatalf("Write: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
}
