// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newTestStorageFS 构造 StorageFS（fake Storage + 临时本地布局）。
func newTestStorageFS(t *testing.T) *StorageFS {
	t.Helper()
	return &StorageFS{
		s:    newFakeStorage(),
		temp: t.TempDir(),
	}
}

func TestStorageFS_ListDir(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	// fake 存两个文件 + 一个子目录
	if _, err := fs.s.Put(context.Background(), "a.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.s.Put(context.Background(), "sub/b.txt", strings.NewReader("world")); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ListDir(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// 单层：顶层含 a.txt（文件）+ sub（目录），不含 sub/b.txt
	byPath := make(map[string]syncpkg.Entry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if _, ok := byPath["a.txt"]; !ok {
		t.Fatalf("顶层缺 a.txt，got paths=%v", pathsOf(entries))
	}
	sub, ok := byPath["sub"]
	if !ok {
		t.Fatalf("顶层缺 sub 目录，got paths=%v", pathsOf(entries))
	}
	if !sub.IsDir {
		t.Fatalf("sub 应为目录，got %+v", sub)
	}
	if _, ok := byPath["sub/b.txt"]; ok {
		t.Fatalf("ListDir 不应递归（出现 sub/b.txt）")
	}
}

func pathsOf(entries []syncpkg.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// TestStorageFS_ListDir_TreeWalk 验证 sync 引擎 WalkEntries 对 StorageFS 递归遍历树。
func TestStorageFS_ListDir_TreeWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(ctx, "a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.s.Put(ctx, "sub/b.txt", strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.s.Put(ctx, "sub/deep/c.txt", strings.NewReader("c")); err != nil {
		t.Fatal(err)
	}
	entries, err := syncpkg.WalkEntries(ctx, fs, "", true, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Path] = true
	}
	// WalkEntries 只对空目录输出目录条目：'sub'、'sub/deep' 均非空（含文件）→ 以子文件体现。
	for _, want := range []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"} {
		if !got[want] {
			t.Fatalf("WalkEntries 缺 %q，got=%v", want, got)
		}
	}
}

func TestStorageFS_Stat_Missing(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	e, err := fs.Stat(context.Background(), "nope.txt")
	if err != nil {
		t.Fatalf("Stat 不存在应返回 (nil,nil)，got err=%v", err)
	}
	if e != nil {
		t.Fatalf("Stat 不存在应返回 nil，got %+v", e)
	}
}

func TestStorageFS_WriteRead(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "f.txt", strings.NewReader("data"), 4, 0); err != nil {
		t.Fatal(err)
	}
	rc, err := fs.OpenRead(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Fatalf("ReadAll = %q, want data", got)
	}
}

func TestStorageFS_Delete(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "f.txt", strings.NewReader("data"), 4, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(context.Background(), "f.txt"); err != nil {
		t.Fatal(err)
	}
	e, err := fs.Stat(context.Background(), "f.txt")
	if err != nil || e != nil {
		t.Fatalf("Delete 后 Stat 应 nil，got e=%+v err=%v", e, err)
	}
}

func TestStorageFS_Rename(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "from.txt", strings.NewReader("data"), 4, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename(context.Background(), "from.txt", "to.txt"); err != nil {
		t.Fatal(err)
	}
	// from 不存在、to 存在
	e, err := fs.Stat(context.Background(), "from.txt")
	if err != nil || e != nil {
		t.Fatalf("Rename 后 from 应 nil，got e=%+v err=%v", e, err)
	}
	rc, err := fs.OpenRead(context.Background(), "to.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "data" {
		t.Fatalf("to.txt = %q, want data", got)
	}
}

var _ syncpkg.FS = (*StorageFS)(nil)

// TestEntryFromMeta_EtagNotMD5 M3 实测回归：百度分片上传后 Stat 的 ETag 是片 md5 组合/
// 服务端标记"可能不正确"，**非整文件 md5**（9MB 实测：本地 8f566ecd vs 远端 18df0a1a）。
// entryFromMeta 必须标 "etag"（与 s3 一致，诚实表达服务端对象标识）——若标 "md5"，
// Equal 的 commonChecksumAlgo 会取 md5 交集比对 → 相同内容恒判不等（误判损坏重下）。
func TestEntryFromMeta_EtagNotMD5(t *testing.T) {
	t.Parallel()
	// 模拟分片上传后 Stat 返回：ETag 是服务端字段（片组合/可能不正确），非本地整文件 md5。
	m := ObjectMeta{Key: "user/big.bin", Size: 9437184, ETag: "18df0a1a48fd23ac82bbc042dcc69bc0"}
	e := entryFromMeta(m)
	if e.ChecksumType != "etag" {
		t.Fatalf("百度 ETag 应标 etag（分片上传非整文件 md5）, got %q", e.ChecksumType)
	}
	if e.Checksums["md5"] != "" {
		t.Fatal("不得把百度 ETag 冒充 md5（Checksums[md5] 应缺失）")
	}
	if got := e.Checksums["etag"]; got != m.ETag {
		t.Fatalf("Checksums[etag] = %q, want %q", got, m.ETag)
	}
}

// TestStorageFS_WriteFile_RejectsTraversalSegments 回归（m8，第 7 轮 MINOR）：路径校验必须
// **逐段**拒绝空段/`.`/`..`——此前只拒前缀 `../`，中段（`docs/../../etc/passwd`）原样透传给
// 远端 API（可能跨目录）。变异：恢复为「只查前缀」→ 中段穿越不再报错 → 红。
func TestStorageFS_WriteFile_RejectsTraversalSegments(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	ctx := context.Background()
	for _, bad := range []string{"../x", "a/../b", "docs/../../etc/passwd", "a//b", "./a", "/"} {
		if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, bad, strings.NewReader("x"), 1, 0); err == nil {
			t.Errorf("路径 %q 应被拒绝（逐段校验）", bad)
		}
	}
	// 合法路径不受影响。
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "docs/a.txt", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("合法路径不应被拒: %v", err)
	}
}

// countFiles 统计目录下文件数与总字节（断言「本地峰值」用）。
func countFiles(t *testing.T, dir string) (int, int64) {
	t.Helper()
	var n int
	var size int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n++
		if fi, ferr := d.Info(); ferr == nil {
			size += fi.Size()
		}
		return nil
	})
	return n, size
}

// TestStorageFS_WriteFile_ReusesCtxStagedPath staging 路径复用（ctx 传递）：调用方已暂存
// （WithStagedLocalPath）时卷侧**零额外落盘**（不复拷第二份），且**只读**该文件（调用方
// 生命周期不受影响）。变异：忽略 ctx 复用分支 → 多出 staging-* 文件 → 红。
func TestStorageFS_WriteFile_ReusesCtxStagedPath(t *testing.T) {
	t.Parallel()
	st := newFakeStorage()
	fs := &StorageFS{s: st, temp: t.TempDir()}
	stagedDir := t.TempDir()
	staged := filepath.Join(stagedDir, "already-staged.bin")
	content := []byte("reuse-me")
	if err := os.WriteFile(staged, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := syncpkg.WithStagedLocalPath(context.Background(), staged)
	// 传入的 reader 在复用分支必须**不被消费**（staging 已完成）。
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "docs/a.bin", strings.NewReader("SHOULD-NOT-BE-USED"), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(复用): %v", err)
	}
	if got := st.files["docs/a.bin"].data; string(got) != string(content) {
		t.Fatalf("复用上传内容应为暂存文件内容, got %q", got)
	}
	if n, _ := countFiles(t, fs.temp); n != 0 {
		t.Fatalf("复用分支不得在卷 staging 目录落任何文件（零额外落盘）, got %d", n)
	}
	if b, rerr := os.ReadFile(staged); rerr != nil || string(b) != string(content) {
		t.Fatalf("暂存文件必须只读（调用方生命周期不受影响）: %q err=%v", b, rerr)
	}
}

// TestStorageFS_WriteFile_SingleLocalStaging 非复用路径只落**一份**本地 staging
// （staging-*），不再由 Storage 内部另拷 stage-*/<basename>（此前峰值 2×size，而 staging
// 配额只按 1× 预留）。变异：改回 f.s.Put(...)（fake 不落盘，但真 Storage 会拷第二份）
// —— 本用例以 fake 记账「调用方 staging 数量」，配合 storage_test 的字节断言双向覆盖。
func TestStorageFS_WriteFile_SingleLocalStaging(t *testing.T) {
	t.Parallel()
	st := newFakeStorage()
	fs := &StorageFS{s: st, temp: t.TempDir()}
	content := strings.Repeat("single-stage-", 64)
	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "docs/b.bin", strings.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := string(st.files["docs/b.bin"].data); got != content {
		t.Fatal("内容不一致")
	}
	// staging 目录在成功返回后应已清空（defer remove），且过程中只有一份。
	if n, _ := countFiles(t, fs.temp); n != 0 {
		t.Fatalf("成功返回后 staging 应清理干净, got %d", n)
	}
}

// TestUploadHintsToPutHints（2026-10-21）：通用 UploadHints → baidupcs 特化 PutHints 映射。
// 断言 RandomContent ⇒ RandomBlob（跳 pre-Stat）+ MD5/CRC32/Size 填充（整块免重扫）；
// 非 RandomContent / 无提示 ⇒ nil（现状自算零回归）。
func TestUploadHintsToPutHints(t *testing.T) {
	t.Parallel()
	// 无提示 ⇒ nil
	if got := uploadHintsToPutHints(context.Background()); got != nil {
		t.Fatalf("无提示应返回 nil，got %+v", got)
	}
	// 非 RandomContent（普通内容）⇒ nil（普通路径按 pre-Stat 秒传命中检查）
	plain := syncpkg.WithUploadHints(context.Background(), &syncpkg.UploadHints{MD5: "m", Size: 9})
	if got := uploadHintsToPutHints(plain); got != nil {
		t.Fatalf("非 RandomContent 应返回 nil（保留 pre-Stat），got %+v", got)
	}
	// RandomContent 全字段 ⇒ 映射齐
	ctx := syncpkg.WithUploadHints(context.Background(), &syncpkg.UploadHints{
		RandomContent: true, Size: 4096, MD5: "deadbeef", CRC32: "12345",
	})
	got := uploadHintsToPutHints(ctx)
	if got == nil || !got.RandomBlob {
		t.Fatalf("RandomContent 应置 RandomBlob，got %+v", got)
	}
	if got.Md5 != "deadbeef" || got.Crc32 != "12345" || got.Size != 4096 {
		t.Fatalf("MD5/CRC32/Size 应填充（免整块重扫），got %+v", got)
	}
	// RandomContent 但无 MD5（外层未算）⇒ 只置 RandomBlob，Size/MD5 留空（内层首次计算补齐）
	ctx2 := syncpkg.WithUploadHints(context.Background(), &syncpkg.UploadHints{RandomContent: true})
	got2 := uploadHintsToPutHints(ctx2)
	if got2 == nil || !got2.RandomBlob || got2.Md5 != "" || got2.Size != 0 {
		t.Fatalf("无 MD5 时只置 RandomBlob（内层自算），got %+v", got2)
	}
}
