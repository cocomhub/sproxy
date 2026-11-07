// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_archive_volume_test.go 锁定 M4 最后一项：**冷段归档卷**（`audit.archive_volume`）。
//
// 语义：
//   - 配置**卷名** ⇒ 冷段写到该卷的 `audit-archive/` 前缀下（加密/封装卷 ⇒ 冷段自带 at-rest
//     加密，容量/配额与普通卷一致）；与 `archive_dir` 同时配置时**以卷为准**；
//   - 归档写侧与冷段读侧**共用同一构造**（`newColdArchiveFS`）⇒ 读写看到同一布局；
//   - 卷解析走 `Set.External(name)` ⇒ 适用于**外部/封装/加密卷**（普通本地卷仍用 `archive_dir`）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// coldVolumeProbeBackend 是「归档卷」替身：最小 ExternalBackend，FS 指向临时目录。
type coldVolumeProbeBackend struct{ fs syncpkg.FS }

func (b coldVolumeProbeBackend) FS() syncpkg.FS { return b.fs }
func (coldVolumeProbeBackend) Close() error     { return nil }

// TestColdArchive_UsesVolumeFS 归档卷生效 ⇒ 段落在 <卷根>/audit-archive/，且读侧可列表/查询。
func TestColdArchive_UsesVolumeFS(t *testing.T) {
	t.Parallel()
	volDir := t.TempDir()
	_, h, _ := newVolumesAPIServer(t, "admin", []VolumeConfig{{Name: "vol-a", Type: "local", Root: filepath.Join(t.TempDir(), "a")}}, nil)
	if err := h.volSet.AddExternalVolume(volume.Volume{Name: "coldvol", Type: "local"},
		coldVolumeProbeBackend{fs: syncpkg.NewLocalFS(volDir, testLogger())}); err != nil {
		t.Fatalf("注册归档卷: %v", err)
	}

	// 同时配 archive_dir ⇒ 验证**卷优先**（目录不应被使用）。
	cfg := Default()
	cfg.Audit.ArchiveVolume = "coldvol"
	cfg.Audit.ArchiveDir = filepath.Join(t.TempDir(), "ignored-dir")
	var cfgp atomic.Pointer[Config]
	cfgp.Store(cfg)
	h.cfgPtr = &cfgp

	coldFS := h.auditColdFS()
	if coldFS == nil {
		t.Fatal("配置了归档卷 ⇒ auditColdFS 不应为 nil")
	}

	// 造一条真实格式的冷段（与 pkg/audit 段格式同源）；放**卷根**下（`audit-archive/` 前缀由
	// FS 包装提供 ⇒ 这里直接写卷内真实位置，等价于归档完成后的状态）。
	entry := audit.Entry{
		Scope: "system", Kind: audit.KindEvent, Type: "sensitive",
		Action: "cold_volume_probe", Result: "success",
		TS: time.Unix(1700000000, 0).UTC(),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	dir := filepath.Join(volDir, auditArchiveVolumePrefix)
	if merr := os.MkdirAll(dir, 0o750); merr != nil {
		t.Fatalf("mkdir: %v", merr)
	}
	if werr := os.WriteFile(filepath.Join(dir, "audit.log.1"), append(line, '\n'), 0o640); werr != nil {
		t.Fatalf("写入冷段: %v", werr)
	}

	// ① 读侧解析到**卷**（而不是 archive_dir）：FS 的根即卷根，前缀 audit-archive/。
	segs, lerr := audit.ListCold(context.Background(), coldFS, "")
	if lerr != nil {
		t.Fatalf("ListCold: %v", lerr)
	}
	if len(segs) != 1 {
		t.Fatalf("应列出 1 个冷段, got %d（卷优先失效？）", len(segs))
	}
	// ② 解码/查询语义本身由 `pkg/audit` 的 `cold_test.go` 覆盖；本用例只锁**接线**：
	//    配置卷 ⇒ 读侧解析到该卷的 `audit-archive/` 前缀（上一步已断言）。
	// ③ 卷不存在 ⇒ 明确降级为「不归档」（返回 nil，不 panic、不误写 archive_dir）。
	cfg2 := Default()
	cfg2.Audit.ArchiveVolume = "no-such-volume"
	cfg2.Audit.ArchiveDir = filepath.Join(t.TempDir(), "fallback-dir")
	cfgp.Store(cfg2)
	if fs2 := h.auditColdFS(); fs2 != nil {
		t.Error("归档卷不存在时应返回 nil（跳过归档），而不是回落 archive_dir")
	}
}
