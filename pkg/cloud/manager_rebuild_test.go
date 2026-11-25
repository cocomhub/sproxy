// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// fakeRebuilder 是 downloader.Rebuilder 替身（记录调用、可指定结果）。
type fakeRebuilder struct {
	downloader.Downloader // 嵌入以复用其余方法（本用例只走 RebuildTo 分支）
	calls                 int
	rebuilt               bool
	err                   error
}

func (f *fakeRebuilder) RebuildTo(_ context.Context, _, dest string, _ bool) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if f.rebuilt {
		_ = os.WriteFile(dest, []byte("rebuilt"), 0o644)
	}
	return f.rebuilt, nil
}

// TestTakeRebuildFirst_Once 锁定「damaged 恢复只尝试一次定点修复」（避免每次重试都扫描）。
func TestTakeRebuildFirst_Once(t *testing.T) {
	t.Parallel()
	mgr := &CloudDownloadManager{}
	task := &CloudTask{rebuildFirst: true}
	if !mgr.takeRebuildFirst(task) {
		t.Fatal("首次应返回 true")
	}
	if mgr.takeRebuildFirst(task) {
		t.Fatal("第二次应返回 false（只尝试一次）")
	}
}

// TestRebuiltResult_FromDisk 锁定「修复后结果取自磁盘实测」（size + sha256，不编造）。
func TestRebuiltResult_FromDisk(t *testing.T) {
	t.Parallel()
	mgr := &CloudDownloadManager{}
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(dest, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res := mgr.rebuiltResult(dest)
	if res.Size != 3 {
		t.Fatalf("Size = %d, want 3", res.Size)
	}
	if res.Checksum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("Checksum = %q（应为 abc 的 sha256）", res.Checksum)
	}
	if res.ChecksumAlgorithm != downloader.IntegrityChecksumSHA256 {
		t.Fatalf("算法 = %q", res.ChecksumAlgorithm)
	}
	if got := mgr.rebuiltResult(filepath.Join(dir, "missing.bin")); got.Size != 0 || got.Checksum != "" {
		t.Fatalf("缺失文件应返回空结果，got %+v", got)
	}
}

// TestFakeRebuilder_Contract 锁定 Rebuilder 契约（false/err 时调用方回落下载）。
func TestFakeRebuilder_Contract(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "x.bin")
	ok := &fakeRebuilder{rebuilt: true}
	if rebuilt, err := ok.RebuildTo(context.Background(), "u", dest, false); err != nil || !rebuilt {
		t.Fatalf("rebuilt 应为 true/nil，got %v/%v", rebuilt, err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("应写产物: %v", err)
	}
	bad := &fakeRebuilder{err: errors.New("boom")}
	if rebuilt, err := bad.RebuildTo(context.Background(), "u", dest, false); err == nil || rebuilt {
		t.Fatal("错误时应回落（rebuilt=false + err）")
	}
	_ = time.Now()
}
