// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ---- 内存 mock FS（测试专用，供 WalkEntries 与 Engine 的符号链接/环场景） ----

type mockEntry struct {
	IsDir     bool
	IsSymlink bool
	Target    string // symlink 目标（IsSymlink=true 时）
	Data      []byte
	MTime     int64
}

type mockFS struct {
	mu      sync.Mutex
	entries map[string]*mockEntry
}

func newMockFS() *mockFS {
	return &mockFS{entries: map[string]*mockEntry{}}
}

func (m *mockFS) setFile(p string, data []byte, mtime int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[p] = &mockEntry{Data: data, MTime: mtime}
	mockEnsureParents(m.entries, p)
}

func (m *mockFS) setDir(p string, mtime int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[p] = &mockEntry{IsDir: true, MTime: mtime}
}

func (m *mockFS) setSymlink(p, target string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[p] = &mockEntry{IsSymlink: true, Target: target}
	mockEnsureParents(m.entries, p)
}

// resolvePath 逐组件解析路径并跟随符号链接（target 为 FS 根相对路径），
// 返回 (条目, 最终解析路径)。深度 64 兜底防自环死循环。
func (m *mockFS) resolvePath(p string) (*mockEntry, string) {
	for range 64 {
		if e, ok := m.entries[p]; ok {
			if !e.IsSymlink {
				return e, p
			}
			p = e.Target
			continue
		}
		if p == "" {
			return &mockEntry{IsDir: true}, ""
		}
		parent := path.Dir(p)
		if parent == p {
			return nil, p
		}
		name := path.Base(p)
		if name == "." || name == "/" {
			return nil, p
		}
		// 父路径可能是符号链接目录：先解析父，再在解析后的父下查子项
		pe, pr := m.resolvePath(parent)
		if pe == nil || !pe.IsDir {
			return nil, p
		}
		child := joinSlash(pr, name)
		if e, ok := m.entries[child]; ok {
			if !e.IsSymlink {
				return e, child
			}
			p = e.Target
			continue
		}
		return nil, p
	}
	return nil, p
}

func (m *mockFS) entry(p string, e *mockEntry) Entry {
	en := Entry{Name: path.Base(p), Path: p, Size: int64(len(e.Data)), MTime: e.MTime, IsDir: e.IsDir, IsSymlink: e.IsSymlink}
	if !e.IsDir && !e.IsSymlink {
		h := sha256.Sum256(e.Data)
		en.Checksum = hex.EncodeToString(h[:])
	}
	return en
}

func (m *mockFS) ListDir(ctx context.Context, p string) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, resolved := m.resolvePath(p)
	if dir == nil {
		return nil, os.ErrNotExist
	}
	if !dir.IsDir {
		return nil, fmt.Errorf("%s 不是目录", p)
	}
	prefix := resolved
	if prefix != "" {
		prefix += "/"
	}
	var out []Entry
	for k, e := range m.entries {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		out = append(out, m.entry(joinSlash(p, rest), e))
	}
	return out, nil
}

func (m *mockFS) Stat(ctx context.Context, p string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, _ := m.resolvePath(p)
	if e == nil {
		return nil, nil
	}
	en := m.entry(p, e)
	return &en, nil
}

func (m *mockFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, _ := m.resolvePath(p)
	if e == nil {
		return nil, os.ErrNotExist
	}
	if e.IsDir {
		return nil, fmt.Errorf("%s 是目录", p)
	}
	return io.NopCloser(bytes.NewReader(e.Data)), nil
}

func (m *mockFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[p] = &mockEntry{Data: data, MTime: mtime}
	mockEnsureParents(m.entries, p)
	return nil
}

func (m *mockFS) Rename(ctx context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[from]
	if !ok {
		return os.ErrNotExist
	}
	delete(m.entries, from)
	m.entries[to] = e
	var toMove []string
	prefix := from + "/"
	for k := range m.entries {
		if strings.HasPrefix(k, prefix) {
			toMove = append(toMove, k)
		}
	}
	for _, k := range toMove {
		child := m.entries[k]
		delete(m.entries, k)
		m.entries[to+"/"+strings.TrimPrefix(k, prefix)] = child
	}
	mockEnsureParents(m.entries, to)
	return nil
}

func (m *mockFS) Delete(ctx context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[p]; !ok {
		return os.ErrNotExist
	}
	delete(m.entries, p)
	prefix := p + "/"
	for k := range m.entries {
		if strings.HasPrefix(k, prefix) {
			delete(m.entries, k)
		}
	}
	return nil
}

func (m *mockFS) MakeDir(ctx context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[p]; ok {
		return nil
	}
	m.entries[p] = &mockEntry{IsDir: true}
	mockEnsureParents(m.entries, p)
	return nil
}

func mockEnsureParents(entries map[string]*mockEntry, p string) {
	dir := path.Dir(p)
	for dir != "" && dir != "." && dir != "/" {
		if _, ok := entries[dir]; !ok {
			entries[dir] = &mockEntry{IsDir: true}
		}
		dir = path.Dir(dir)
	}
}

// walkPaths 提取条目 Path 列表（已排序）。
func walkPaths(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

// ---- WalkEntries 测试 ----

func TestWalkEntries_Recursive(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("a"), 1)
	m.setFile("sub/b.txt", []byte("b"), 2)
	m.setFile("sub/deep/c.txt", []byte("c"), 3)

	entries, err := WalkEntries(context.Background(), m, "", true, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("递归枚举路径不符\n got: %v\nwant: %v", got, want)
	}
}

func TestWalkEntries_NonRecursive(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("a"), 1)
	m.setFile("sub/b.txt", []byte("b"), 2)

	entries, err := WalkEntries(context.Background(), m, "", false, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"a.txt", "sub"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("非递归枚举路径不符\n got: %v\nwant: %v", got, want)
	}
	// sub 应为目录条目
	for _, e := range entries {
		if e.Path == "sub" && !e.IsDir {
			t.Fatalf("sub 应为目录条目")
		}
	}
}

func TestWalkEntries_Filters(t *testing.T) {
	m := newMockFS()
	m.setFile("a.go", []byte("a"), 1)
	m.setFile("b.tmp", []byte("b"), 2)
	m.setFile("c.txt", []byte("c"), 3)

	filters := ParseFilters(nil, []string{"*.tmp"})
	entries, err := WalkEntries(context.Background(), m, "", true, false, filters)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"a.go", "c.txt"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("exclude 过滤不符\n got: %v\nwant: %v", got, want)
	}

	filters2 := ParseFilters([]string{"*.go"}, nil)
	entries2, err := WalkEntries(context.Background(), m, "", true, false, filters2)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want2 := []string{"a.go"}
	if got := walkPaths(entries2); !reflect.DeepEqual(got, want2) {
		t.Fatalf("include 过滤不符\n got: %v\nwant: %v", got, want2)
	}
}

func TestWalkEntries_InternalDirSkipped(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("a"), 1)
	m.setFile(".__internal__/x.txt", []byte("x"), 2)

	entries, err := WalkEntries(context.Background(), m, "", true, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"a.txt"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("内部目录应被跳过\n got: %v\nwant: %v", got, want)
	}
}

func TestWalkEntries_EmptyDirEmitted(t *testing.T) {
	m := newMockFS()
	m.setDir("empty", 1)
	m.setFile("a.txt", []byte("a"), 2)

	entries, err := WalkEntries(context.Background(), m, "", true, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"a.txt", "empty"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("空目录应被枚举为目录条目\n got: %v\nwant: %v", got, want)
	}
	for _, e := range entries {
		if e.Path == "empty" && !e.IsDir {
			t.Fatalf("empty 应为目录条目")
		}
	}
}

func TestWalkEntries_RootFile(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("hello"), 123)

	entries, err := WalkEntries(context.Background(), m, "a.txt", true, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("单文件根应返回 1 个条目，got %d", len(entries))
	}
	if entries[0].Path != "a.txt" || entries[0].IsDir {
		t.Fatalf("单文件条目不符: %+v", entries[0])
	}
	if entries[0].Size != 5 {
		t.Fatalf("Size 应为 5，got %d", entries[0].Size)
	}
}

func TestWalkEntries_RootMissing(t *testing.T) {
	m := newMockFS()
	_, err := WalkEntries(context.Background(), m, "nope", true, false, nil)
	if err == nil {
		t.Fatalf("源路径不存在应返回 error")
	}
}

func TestWalkEntries_SymlinkSkip(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("a"), 1)
	m.setSymlink("link", "a.txt")

	entries, err := WalkEntries(context.Background(), m, "", true, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	var link *Entry
	for i := range entries {
		if entries[i].Path == "link" {
			link = &entries[i]
		}
	}
	if link == nil {
		t.Fatalf("符号链接应被枚举（由引擎决定跳过），entries=%v", walkPaths(entries))
	}
	if !link.IsSymlink {
		t.Fatalf("符号链接条目 IsSymlink 应为 true")
	}
}

func TestWalkEntries_SymlinkFollowFile(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("hello"), 5)
	m.setSymlink("link", "a.txt")

	entries, err := WalkEntries(context.Background(), m, "", true, true, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	var link *Entry
	for i := range entries {
		if entries[i].Path == "link" {
			link = &entries[i]
		}
	}
	if link == nil {
		t.Fatalf("跟随符号链接后应产出条目")
	}
	if link.IsSymlink {
		t.Fatalf("跟随符号链接后 IsSymlink 应为 false")
	}
	if link.Size != 5 {
		t.Fatalf("跟随符号链接后 Size 应为目标大小 5，got %d", link.Size)
	}
	if link.Checksum == "" {
		t.Fatalf("跟随符号链接后应有 checksum")
	}
}

func TestWalkEntries_SymlinkFollowDir(t *testing.T) {
	m := newMockFS()
	m.setFile("real/b.txt", []byte("b"), 2)
	m.setDir("sub", 1)
	m.setSymlink("sub/dirlink", "real")

	// walk root=sub：目标 real 不在枚举范围内，只能经 dirlink 跟随到达
	entries, err := WalkEntries(context.Background(), m, "sub", true, true, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"sub/dirlink/b.txt"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("跟随目录符号链接应产出目标子项\n got: %v\nwant: %v", got, want)
	}
}

func TestWalkEntries_SymlinkSelfLoop(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("a"), 1)
	m.setSymlink("self", "self")

	// 自环符号链接在 follow=true 时不得死循环（resolve 深度兜底 → 记为符号链接条目，引擎跳过）
	entries, err := WalkEntries(context.Background(), m, "", true, true, nil)
	if err != nil {
		t.Fatalf("自环符号链接不应导致 WalkEntries error: %v", err)
	}
	for _, e := range entries {
		if e.Path == "self" && !e.IsSymlink {
			t.Fatalf("无法解析的自环符号链接应保留 IsSymlink=true")
		}
	}
}

func TestWalkEntries_SymlinkGrowingCycle(t *testing.T) {
	m := newMockFS()
	m.setDir("sub", 1)
	m.setSymlink("sub/loop", "sub")

	// 不断增长的符号链接目录环应被深度上限截断并返回 error（fail-closed），不得死循环
	_, err := WalkEntries(context.Background(), m, "", true, true, nil)
	if err == nil {
		t.Fatalf("符号链接目录环应返回 error")
	}
	if !strings.Contains(err.Error(), "深度") {
		t.Fatalf("错误信息应提示目录深度超限，got %v", err)
	}
}

// TestWalkEntries_ListDirError 验证 ListDir 失败时返回 error。
func TestWalkEntries_ListDirError(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("a"), 1)
	m.setDir("blocked", 2)

	bf := &errorFS{FS: m, listDirErr: errors.New("权限不足")}
	_, err := WalkEntries(context.Background(), bf, "", true, false, nil)
	if err == nil {
		t.Fatalf("ListDir 失败应返回 error")
	}
}

// errorFS 包装 mockFS，在 ListDir 上注入错误。
type errorFS struct {
	FS
	listDirErr error
}

func (e *errorFS) ListDir(ctx context.Context, p string) ([]Entry, error) {
	return nil, e.listDirErr
}

// TestWalkEntries_SymlinkFollowDir_NonRecursive 验证跟随目录符号链接 + recursive=false
// 分支：目录符号链接解析后作为目录条目返回（审查 M9）。
func TestWalkEntries_SymlinkFollowDir_NonRecursive(t *testing.T) {
	m := newMockFS()
	m.setFile("real/b.txt", []byte("b"), 2)
	m.setDir("sub", 1)
	m.setSymlink("sub/dirlink", "real")

	entries, err := WalkEntries(context.Background(), m, "sub", false, true, nil)
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"sub/dirlink"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("非递归跟随目录符号链接应产出目录条目\n got: %v\nwant: %v", got, want)
	}
	for _, e := range entries {
		if e.Path == "sub/dirlink" && !e.IsDir {
			t.Fatalf("sub/dirlink 应为目录条目，got %+v", e)
		}
	}
}
