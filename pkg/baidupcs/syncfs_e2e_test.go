// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestSyncEngine_LocalToBaidu 端到端：pkg/sync 引擎对「本地 fs ↔ StorageFS（fake 网盘）」
// 跑一次 push 同步，断言文件往返一致 + 结果 action=created。
func TestSyncEngine_LocalToBaidu(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srcRoot := t.TempDir()
	writeLocal(t, srcRoot, "a.txt", "hello")
	writeLocal(t, srcRoot, "sub/b.txt", "world")

	// dst = StorageFS（fake 网盘）
	fs, err := NewStorageFS(newFakeStorage(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	job := &syncpkg.Job{
		Direction:      syncpkg.DirectionPush,
		Src:            "",
		Dst:            "",
		Recursive:      true,
		ConflictPolicy: syncpkg.ConflictSkip,
	}
	engine := &syncpkg.Engine{Concurrency: 2}
	if err := engine.Sync(ctx, syncpkg.NewLocalFS(srcRoot, nil), fs, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if job.Status != syncpkg.StatusCompleted {
		t.Fatalf("Status = %q, want completed", job.Status)
	}
	if job.Stats.FilesTotal != 2 || job.Stats.FilesDone != 2 {
		t.Fatalf("FilesTotal/Done = %d/%d, want 2/2", job.Stats.FilesTotal, job.Stats.FilesDone)
	}
	// 往返一致：dst 读回内容与 src 相同
	rc, err := fs.OpenRead(ctx, "a.txt")
	if err != nil {
		t.Fatalf("OpenRead a.txt: %v", err)
	}
	var sb strings.Builder
	buf := make([]byte, 32)
	for {
		n, rerr := rc.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	rc.Close()
	if sb.String() != "hello" {
		t.Fatalf("a.txt 往返 = %q, want hello", sb.String())
	}
	// 结果 action=created
	if len(job.Results) != 2 {
		t.Fatalf("Results = %d, want 2", len(job.Results))
	}
	for _, r := range job.Results {
		if r.Action != syncpkg.ActionCreated {
			t.Fatalf("action = %q, want created (%+v)", r.Action, r)
		}
	}
}

// TestSyncEngine_BaiduToLocal 端到端反向：网盘 → 本地 pull。
func TestSyncEngine_BaiduToLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs, err := NewStorageFS(newFakeStorage(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 预置网盘文件
	if err := fs.WriteFile(ctx, "x.txt", strings.NewReader("netdisk"), 7, 0); err != nil {
		t.Fatal(err)
	}
	dstRoot := t.TempDir()
	job := &syncpkg.Job{
		Direction:      syncpkg.DirectionPush,
		Src:            "",
		Dst:            "",
		Recursive:      true,
		ConflictPolicy: syncpkg.ConflictSkip,
	}
	engine := &syncpkg.Engine{Concurrency: 2}
	if err := engine.Sync(ctx, fs, syncpkg.NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// dst 本地出现 x.txt
	data, err := readLocalFile(dstRoot, "x.txt")
	if err != nil {
		t.Fatalf("read x.txt: %v", err)
	}
	if string(data) != "netdisk" {
		t.Fatalf("x.txt = %q, want netdisk", string(data))
	}
}

// writeLocal 写本地测试文件（sync 测试用）。
func writeLocal(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := writeLocalFile(root, rel, content); err != nil {
		t.Fatal(err)
	}
}

// writeLocalFile 用 os 写文件（跨平台路径）。
func writeLocalFile(root, rel, content string) error {
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// readLocalFile 读本地文件。
func readLocalFile(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
}
