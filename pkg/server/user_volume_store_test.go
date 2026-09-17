// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// user_volume_store_test.go 钉住用户卷 meta store（U2）：每 owner 的 volume 持久化
// （<storage_root>/<owner>/meta/volume/<name>.json，原子写）+ 扫描恢复。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// newTestUserVolumeStore 构造绑定到 t.TempDir() 的 store（owner 目录隔离）。
func newTestUserVolumeStore(t *testing.T) *UserVolumeStore {
	t.Helper()
	return NewUserVolumeStore(t.TempDir())
}

// TestUserVolumeStore_CreateGet 验证 Create → Get 往返（内容一致）。
func TestUserVolumeStore_CreateGet(t *testing.T) {
	t.Parallel()
	s := newTestUserVolumeStore(t)
	v := UserVolume{Name: "my-disk-1", Type: "baidupcs", Owner: "alice", Capacity: 1 << 30,
		Extra: map[string]any{"bduss": "test", "baidu_root": "/disk1"}}
	if err := s.Create("alice", v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get("alice", "my-disk-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get 返回 nil，want 非 nil")
	}
	if got.Name != v.Name || got.Type != v.Type || got.Owner != v.Owner || got.Capacity != v.Capacity {
		t.Fatalf("往返不一致: got %+v, want %+v", got, v)
	}
	if got.Extra["bduss"] != "test" || got.Extra["baidu_root"] != "/disk1" {
		t.Fatalf("Extra 不一致: %v", got.Extra)
	}
}

// TestUserVolumeStore_Get_Missing 验证不存在的卷返回 (nil, nil)。
func TestUserVolumeStore_Get_Missing(t *testing.T) {
	t.Parallel()
	s := newTestUserVolumeStore(t)
	got, err := s.Get("alice", "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("Get 缺失 = %+v, want nil", got)
	}
}

// TestUserVolumeStore_Create_InvalidName 验证非法卷名拒绝（路径穿越等）。
func TestUserVolumeStore_Create_InvalidName(t *testing.T) {
	t.Parallel()
	s := newTestUserVolumeStore(t)
	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a:b"} {
		v := UserVolume{Name: name, Type: "baidupcs", Owner: "alice"}
		if err := s.Create("alice", v); err == nil {
			t.Fatalf("Create(name=%q) 应拒绝，got nil", name)
		}
	}
}

// TestUserVolumeStore_Create_Duplicate 验证重名拒绝（同 owner）。
func TestUserVolumeStore_Create_Duplicate(t *testing.T) {
	t.Parallel()
	s := newTestUserVolumeStore(t)
	v := UserVolume{Name: "d1", Type: "baidupcs", Owner: "alice"}
	if err := s.Create("alice", v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create("alice", v); err == nil {
		t.Fatal("重名 Create 应拒绝")
	}
}

// TestUserVolumeStore_ListByOwner 验证每 owner 隔离（只列自己的）。
func TestUserVolumeStore_ListByOwner(t *testing.T) {
	t.Parallel()
	s := newTestUserVolumeStore(t)
	for _, tc := range []struct{ owner, name string }{
		{"alice", "a1"}, {"alice", "a2"}, {"bob", "b1"},
	} {
		if err := s.Create(tc.owner, UserVolume{Name: tc.name, Type: "baidupcs", Owner: tc.owner}); err != nil {
			t.Fatalf("Create(%s/%s): %v", tc.owner, tc.name, err)
		}
	}
	alice, err := s.ListByOwner("alice")
	if err != nil {
		t.Fatalf("ListByOwner(alice): %v", err)
	}
	names := make([]string, 0, len(alice))
	for _, v := range alice {
		names = append(names, v.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "a1" || names[1] != "a2" {
		t.Fatalf("alice 卷 = %v, want [a1 a2]", names)
	}
	bob, err := s.ListByOwner("bob")
	if err != nil {
		t.Fatalf("ListByOwner(bob): %v", err)
	}
	if len(bob) != 1 || bob[0].Name != "b1" {
		t.Fatalf("bob 卷 = %+v, want [b1]", bob)
	}
}

// TestUserVolumeStore_Delete 验证删除 → Get nil。
func TestUserVolumeStore_Delete(t *testing.T) {
	t.Parallel()
	s := newTestUserVolumeStore(t)
	v := UserVolume{Name: "del-me", Type: "baidupcs", Owner: "alice"}
	if err := s.Create("alice", v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete("alice", "del-me"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Get("alice", "del-me")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("Delete 后 Get = %+v, want nil", got)
	}
}

// TestUserVolumeStore_Delete_Missing 验证删除不存在 → 明确错误。
func TestUserVolumeStore_Delete_Missing(t *testing.T) {
	t.Parallel()
	s := newTestUserVolumeStore(t)
	if err := s.Delete("alice", "nope"); err == nil {
		t.Fatal("Delete 缺失应报错")
	}
}

// TestUserVolumeStore_AtomicWrite 验证原子写落盘（Create 后文件存在且为 JSON，可重新加载）。
func TestUserVolumeStore_AtomicWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := NewUserVolumeStore(root)
	v := UserVolume{Name: "atomic-1", Type: "baidupcs", Owner: "alice", Capacity: 10}
	if err := s.Create("alice", v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 落盘位置：<root>/alice/meta/volume/atomic-1.json
	diskPath := filepath.Join(root, "alice", "meta", "volume", "atomic-1.json")
	data, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	if !strings.Contains(string(data), `"atomic-1"`) || !strings.Contains(string(data), `"baidupcs"`) {
		t.Fatalf("落盘 JSON 缺字段: %s", data)
	}
	// 重新 New store 加载（模拟重启后新实例读取既有文件）
	s2 := NewUserVolumeStore(root)
	got, err := s2.Get("alice", "atomic-1")
	if err != nil {
		t.Fatalf("新实例 Get: %v", err)
	}
	if got == nil || got.Capacity != 10 {
		t.Fatalf("新实例读取 = %+v, want capacity=10", got)
	}
}

// TestUserVolumeStore_ScanRestore 验证跨 owner 扫描恢复（重启装配用）。
func TestUserVolumeStore_ScanRestore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := NewUserVolumeStore(root)
	for _, tc := range []struct{ owner, name string }{
		{"alice", "a1"}, {"alice", "a2"}, {"bob", "b1"},
	} {
		if err := s.Create(tc.owner, UserVolume{Name: tc.name, Type: "baidupcs", Owner: tc.owner}); err != nil {
			t.Fatalf("Create(%s/%s): %v", tc.owner, tc.name, err)
		}
	}
	// 新实例扫描恢复
	s2 := NewUserVolumeStore(root)
	vols, err := s2.ScanRestore()
	if err != nil {
		t.Fatalf("ScanRestore: %v", err)
	}
	if len(vols) != 3 {
		t.Fatalf("恢复卷数 = %d, want 3", len(vols))
	}
	owners := map[string]int{}
	for _, v := range vols {
		owners[v.Owner]++
	}
	if owners["alice"] != 2 || owners["bob"] != 1 {
		t.Fatalf("按 owner 恢复数 = %v, want alice:2 bob:1", owners)
	}
}
