// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

// local_pull_meta_test.go 覆盖 sync pull/both 本地落盘的 meta sidecar 联动（第 5 轮
// 对抗评审 P1：此前 `pkg/syncexec` 全包零 meta 引用——覆盖写遗下陈旧 sidecar 使
// `/download` 永久 fail-closed；新文件无 sidecar 则下载静默零校验）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// TestNewLocalPullFS_WritesSidecarUnderMetaBucket：pull 目标 FS 写文件后，sidecar 必须
// 落在**租户根**的 meta 桶（`<tenantRoot>/meta/<rel>.meta`），而不是 user 目录内的裸
// `.meta`（无桶段兜底），且哈希与内容一致。
func TestNewLocalPullFS_WritesSidecarUnderMetaBucket(t *testing.T) {
	t.Parallel()
	tenantRoot := t.TempDir()
	e := NewExecutor(nil, nil)
	fs := e.newLocalPullFS(tenantRoot)
	ctx := context.Background()

	content := []byte("pulled-content-0123456789")
	if err := fs.WriteFile(ctx, "docs/a.txt", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 主文件在 user 桶。
	if _, err := os.Stat(filepath.Join(tenantRoot, "user", "docs", "a.txt")); err != nil {
		t.Fatalf("主文件应落 user 桶: %v", err)
	}
	// sidecar 在 meta 桶、可解析、哈希自洽。
	sidecar := filepath.Join(tenantRoot, "meta", "docs", "a.txt.meta")
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("sidecar 应落 meta 桶（非 user 目录内 .meta）: %v", err)
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatalf("sidecar 反序列化: %v", uerr)
	}
	if err := meta.Validate(fm); err != nil {
		t.Fatalf("sidecar Validate: %v", err)
	}
	sum := sha256.Sum256(content)
	if fm.TotalSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sidecar TotalSHA256=%s 与内容不符", fm.TotalSHA256)
	}
	// 不得在 user 目录内留下裸 .meta（无桶段兜底路径）。
	if _, err := os.Stat(filepath.Join(tenantRoot, "user", "docs", "a.txt.meta")); err == nil {
		t.Fatal("sidecar 不得落在 user 桶用户可见目录")
	}
}

// TestNewLocalPullFS_OverwriteRefreshesSidecar：覆盖写（pull 更新已存在文件）后 sidecar
// 必须描述**新**内容——否则读校验按旧哈希比对新内容恒失配（文件永久不可下载）。
func TestNewLocalPullFS_OverwriteRefreshesSidecar(t *testing.T) {
	t.Parallel()
	tenantRoot := t.TempDir()
	e := NewExecutor(nil, nil)
	fs := e.newLocalPullFS(tenantRoot)
	ctx := context.Background()

	old := []byte("old-content")
	if err := fs.WriteFile(ctx, "a.txt", bytes.NewReader(old), int64(len(old)), 0); err != nil {
		t.Fatal(err)
	}
	newer := []byte("newer-content-longer-than-before")
	if err := fs.WriteFile(ctx, "a.txt", bytes.NewReader(newer), int64(len(newer)), 0); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tenantRoot, "meta", "a.txt.meta"))
	if err != nil {
		t.Fatal(err)
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		t.Fatal(uerr)
	}
	sum := sha256.Sum256(newer)
	if fm.TotalSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("覆盖写后 sidecar 仍描述旧内容: %s", fm.TotalSHA256)
	}
	if fm.Size != int64(len(newer)) {
		t.Fatalf("sidecar Size=%d want %d", fm.Size, len(newer))
	}
	// Delete 应联动清理 sidecar（镜像删除方向）。
	if err := fs.Delete(ctx, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tenantRoot, "meta", "a.txt.meta")); err == nil {
		t.Fatal("Delete 应联动清理 sidecar（否则容量/磁盘泄漏）")
	}
}
