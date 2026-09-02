// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package file_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/store"
	"github.com/cocomhub/sproxy/pkg/store/file"
)

func TestFileStore_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	st, err := file.New(store.StoreConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Set("k/v", []byte("value")); err != nil {
		t.Fatal(err)
	}
	data, err := st.Get("k/v")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value" {
		t.Fatalf("got %q", data)
	}
	// 无 tmp 残留
	if _, err = os.Stat(filepath.Join(dir, "k", "v.tmp")); !os.IsNotExist(err) {
		t.Fatalf("不应残留 tmp 文件")
	}
}

func TestFileStore_CrashResidueCleaned(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "k"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "k", "v.tmp"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := file.New(store.StoreConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Get("k/v")
	if !os.IsNotExist(err) {
		t.Fatalf("残留 tmp 应不影响读取, err=%v", err)
	}
}

// TestFileStore_GetMissing 验证 Get 不存在 key 返回 os.ErrNotExist。
func TestFileStore_GetMissing(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Get("nope")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get(不存在) err=%v want os.ErrNotExist", err)
	}
}

// TestFileStore_RejectUnsafeKeys 验证 key 安全校验拒绝危险 key（../、绝对路径、空段、反斜杠）。
func TestFileStore_RejectUnsafeKeys(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	unsafe := []string{
		"",            // 空 key
		"/abs",        // 绝对路径
		`\abs`,        // Windows 绝对路径
		"a/../b",      // 父目录逃逸
		"..",          // 根逃逸
		"../x",        // 根逃逸
		"a/./b",       // 点段
		"a//b",        // 空段
		"a/b/",        // 尾空段
		"a\\b",        // 段内反斜杠
		"a/b\\c",      // 段内反斜杠
		"a.tmp",       // .tmp 保留后缀（会被 List 当临时残留跳过）
		"cloud/a.tmp", // .tmp 保留后缀（会被 Set("cloud/a") 的临时文件覆盖）
	}
	for _, key := range unsafe {
		if err = st.Set(key, []byte("x")); err == nil {
			t.Errorf("Set(%q) 应返回错误", key)
		}
	}
}

// TestFileStore_ListNested 验证前缀遍历递归返回文件并跳过 tmp 残留；空前缀 = 遍历整根。
func TestFileStore_ListNested(t *testing.T) {
	dir := t.TempDir()
	st, err := file.New(store.StoreConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Set("cloud/a", []byte("A")); err != nil {
		t.Fatal(err)
	}
	if err = st.Set("cloud/groups/g1", []byte("G1")); err != nil {
		t.Fatal(err)
	}
	if err = st.Set("sync/c", []byte("C")); err != nil {
		t.Fatal(err)
	}
	// 直接写一个 tmp 残留到 cloud 目录下，验证 List 跳过它
	if err = os.WriteFile(filepath.Join(dir, "cloud", "junk.tmp"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := st.List("cloud/")
	if err != nil {
		t.Fatal(err)
	}
	// cloud/a 与 cloud/groups/g1 两条记录；junk.tmp 应被跳过
	if len(items) != 2 {
		t.Fatalf("List(cloud/)=%d want 2 (跳过 tmp), items=%v", len(items), items)
	}
	// 空前缀 = 遍历整根，应含 3 条记录
	all, err := st.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List(\"\")=%d want 3", len(all))
	}
}

// TestFileStore_ListEmptyPrefixMissingDir 验证前缀目录不存在时返回空结果而非错误。
func TestFileStore_ListEmptyPrefixMissingDir(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.List("not/exist/")
	if err != nil {
		t.Fatalf("List(不存在的目录) err=%v want nil", err)
	}
	if len(items) != 0 {
		t.Fatalf("List(不存在的目录)=%d want 0", len(items))
	}
}

// TestFileStore_ListAbsolutePrefix 验证绝对路径前缀（/ 或 \ 开头）返回错误。
func TestFileStore_ListAbsolutePrefix(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/", "/abs", `\`, `\abs`} {
		if _, err = st.List(prefix); err == nil {
			t.Errorf("List(%q) 应返回错误", prefix)
		}
	}
}

// TestFileStore_DeleteIdempotent 验证删除不存在的 key 幂等（不报错）。
func TestFileStore_DeleteIdempotent(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Set("a", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err = st.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err = st.Delete("a"); err != nil {
		t.Fatalf("重复删除应幂等, err=%v", err)
	}
	if err = st.Delete("never-existed"); err != nil {
		t.Fatalf("删除不存在 key 应幂等, err=%v", err)
	}
}

// TestFileStore_Close 验证 Close 不报错（file 无资源需释放）。
func TestFileStore_Close(t *testing.T) {
	st, err := file.New(store.StoreConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestFileStore_NewEmptyRoot 验证空 Root 返回错误。
func TestFileStore_NewEmptyRoot(t *testing.T) {
	if _, err := file.New(store.StoreConfig{}); err == nil {
		t.Fatal("New(空 Root) 应返回错误")
	}
}
