// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// 本文件锁定设计 docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md
// §9.2（本地临时目录确定性命名：复用而非堆积 + 并发串行化 + 安全 0700/不跟随符号链接）。

// TestChunkTempDirName_Deterministic：命名规则 = <kind><pid>-<sha256(key)[:16]>。
// 同 key 同名、不同 key 不同名、不同 kind 前缀区分。
func TestChunkTempDirName_Deterministic(t *testing.T) {
	t.Parallel()
	a := chunkTempDirName(chunkTempPrefix, "jav/maker/file.mp4")
	b := chunkTempDirName(chunkTempPrefix, "jav/maker/file.mp4")
	if a != b {
		t.Fatalf("同 key 必须同名: %s vs %s", a, b)
	}
	if !strings.HasPrefix(a, chunkTempPrefix) {
		t.Fatalf("目录名必须带确定性前缀 %q: %s", chunkTempPrefix, a)
	}
	if c := chunkTempDirName(chunkTempPrefix, "jav/maker/other.mp4"); c == a {
		t.Fatalf("不同 key 必须不同名: %s", a)
	}
	if chunkTempDirName(dedupTempPrefix, "x") == chunkTempDirName(chunkTempPrefix, "x") {
		t.Fatal("不同 kind 前缀必须区分目录")
	}
}

// TestNewChunkTempDir_ReuseSamePath：同一任务（同 key）两次取临时目录 ⇒ **同路径**。
// 随机 MkdirTemp 时两次不同 ⇒ 目录堆积（本用例红）。
func TestNewChunkTempDir_ReuseSamePath(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	d1, r1, err := fs.newChunkTempDir(chunkTempPrefix, "a/b.bin")
	if err != nil {
		t.Fatalf("newChunkTempDir(1): %v", err)
	}
	r1()
	d2, r2, err := fs.newChunkTempDir(chunkTempPrefix, "a/b.bin")
	if err != nil {
		t.Fatalf("newChunkTempDir(2): %v", err)
	}
	r2()
	if d1 != d2 {
		t.Fatalf("同任务必须复用同目录（确定性命名）: %s vs %s", d1, d2)
	}
	if filepath.Dir(d1) != fs.temp {
		t.Fatalf("临时目录必须落在卷临时根下: %s (temp=%s)", d1, fs.temp)
	}
}

// TestNewChunkTempDir_DifferentKeysDifferentDirs：不同任务 ⇒ 不同目录（天然并行）。
func TestNewChunkTempDir_DifferentKeysDifferentDirs(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	d1, r1, err := fs.newChunkTempDir(chunkTempPrefix, "a/one.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	d2, r2, err := fs.newChunkTempDir(chunkTempPrefix, "a/two.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
	if d1 == d2 {
		t.Fatalf("不同 key 必须不同目录: %s", d1)
	}
}

// TestNewChunkTempDir_ReleasedAndCleaned：释放后目录被删除；复用同 key 时清空上次残留。
func TestNewChunkTempDir_ReleasedAndCleaned(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	d, r, err := fs.newChunkTempDir(chunkTempPrefix, "a/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(d, "stale-chunk")
	if werr := os.WriteFile(stale, []byte("x"), 0o600); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	r()
	if _, serr := os.Stat(d); !os.IsNotExist(serr) {
		t.Fatalf("释放后目录应被删除: %v", serr)
	}
	d2, r2, err := fs.newChunkTempDir(chunkTempPrefix, "a/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
	if d2 != d {
		t.Fatalf("复用同 key 应同目录: %s vs %s", d2, d)
	}
	if _, serr := os.Stat(stale); !os.IsNotExist(serr) {
		t.Fatalf("复用目录必须清空上次残留（否则误上传陈旧分块）: %v", serr)
	}
}

// TestNewChunkTempDir_Permission0700：目录权限必须为 0700（不放宽）。
func TestNewChunkTempDir_Permission0700(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不保留 POSIX 权限位")
	}
	fs := newFS(t)
	d, r, err := fs.newChunkTempDir(chunkTempPrefix, "a/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r()
	fi, serr := os.Stat(d)
	if serr != nil {
		t.Fatal(serr)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("临时目录权限必须为 0700，实际 %o", fi.Mode().Perm())
	}
}

// TestNewChunkTempDir_RejectsSymlink：确定性路径被符号链接占用 ⇒ 拒绝（不跟随）。
func TestNewChunkTempDir_RejectsSymlink(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	dir := filepath.Join(fs.temp, chunkTempDirName(chunkTempPrefix, "a/b.bin"))
	if derr := os.MkdirAll(fs.temp, 0o700); derr != nil {
		t.Fatal(derr)
	}
	if serr := os.Symlink(t.TempDir(), dir); serr != nil {
		t.Skipf("无法创建符号链接（权限/平台）: %v", serr)
	}
	if _, _, err := fs.newChunkTempDir(chunkTempPrefix, "a/b.bin"); err == nil {
		t.Fatal("确定性临时目录被符号链接占用时必须拒绝（O_NOFOLLOW 语义）")
	}
}

// TestNewChunkTempDir_SerializesSameKey：同 key 并发写者必须串行化（第二写者阻塞到第一释放）。
func TestNewChunkTempDir_SerializesSameKey(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	const key = "a/b.bin"
	acquired := make(chan struct{})
	hold := make(chan struct{})
	go func() {
		d, r, err := fs.newChunkTempDir(chunkTempPrefix, key)
		if err != nil {
			close(acquired)
			return
		}
		_ = d
		close(acquired)
		<-hold
		r()
	}()
	<-acquired
	got := make(chan struct{})
	go func() {
		d2, r2, err := fs.newChunkTempDir(chunkTempPrefix, key)
		if err != nil {
			close(got)
			return
		}
		_ = d2
		close(got)
		r2()
	}()
	select {
	case <-got:
		t.Fatal("同 key 第二写者在第一写者释放前即取得目录 ⇒ 未串行化")
	case <-time.After(150 * time.Millisecond):
	}
	close(hold)
	select {
	case <-got:
	case <-time.After(testutil.ScaleTimeout(2 * time.Second)):
		t.Fatal("第一写者释放后第二写者应能取得目录")
	}
}
