// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// state_adapter_volume_test.go 钉住集群后端（StateStore）的**卷维度键**（设计 §15.1.1）：
// `checksumSet/Get` 按 `VolumeKeyedStore` 断言选择卷维度路径，后端未实现时静默退化为
// 无卷键 ⇒ 同一 owner 多卷同名文件互相覆盖（checksum 张冠李戴）。

import (
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// TestStateBackedStore_VolumeKeyedIsolation：两卷同名 rel 各自保留 checksum。
//
// 变异：`SetInVolume` 改为 `s.Set(filename, checksum)`（无卷键）⇒ 后写覆盖前写 ⇒ 红。
func TestStateBackedStore_VolumeKeyedIsolation(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())

	s.SetInVolume("volA", "user/a.txt", "aaaa")
	s.SetInVolume("volB", "user/a.txt", "bbbb")

	if got, ok := s.GetInVolume("volA", "user/a.txt"); !ok || got != "aaaa" {
		t.Fatalf("volA 应保留自己的 checksum, got=%q ok=%v", got, ok)
	}
	if got, ok := s.GetInVolume("volB", "user/a.txt"); !ok || got != "bbbb" {
		t.Fatalf("volB 应保留自己的 checksum, got=%q ok=%v", got, ok)
	}
	if got := s.SnapshotInVolume("volA"); len(got) != 1 || got["user/a.txt"] != "aaaa" {
		t.Fatalf("SnapshotInVolume(volA)=%v", got)
	}
	if got := s.SnapshotLegacy(); len(got) != 0 {
		t.Fatalf("SnapshotLegacy 不应含卷维度键, got %v", got)
	}

	// 落盘后重载仍隔离（卷维度键随快照持久化）。
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	s2 := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())
	if got, _ := s2.GetInVolume("volA", "user/a.txt"); got != "aaaa" {
		t.Fatalf("重载后 volA checksum=%q want aaaa", got)
	}
	if got, _ := s2.GetInVolume("volB", "user/a.txt"); got != "bbbb" {
		t.Fatalf("重载后 volB checksum=%q want bbbb", got)
	}
}

// TestStateBackedStore_VolumeRenameAndDelete：同卷重命名/删除/前缀清理只影响目标卷。
func TestStateBackedStore_VolumeRenameAndDelete(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())

	s.SetInVolume("volA", "user/dir/a.txt", "cs-a")
	s.SetInVolume("volB", "user/dir/a.txt", "cs-b")
	s.RenameInVolume("volA", "user/dir/a.txt", "user/dir/b.txt")
	if _, ok := s.GetInVolume("volA", "user/dir/a.txt"); ok {
		t.Fatal("volA 旧键应已重命名")
	}
	if got, ok := s.GetInVolume("volA", "user/dir/b.txt"); !ok || got != "cs-a" {
		t.Fatalf("volA 新键=%q,%v", got, ok)
	}
	if got, ok := s.GetInVolume("volB", "user/dir/a.txt"); !ok || got != "cs-b" {
		t.Fatalf("volA 重命名不得影响 volB, got=%q,%v", got, ok)
	}
	s.DeletePrefixInVolume("volB", "user/dir/")
	if _, ok := s.GetInVolume("volB", "user/dir/a.txt"); ok {
		t.Fatal("volB 前缀清理应删除该条")
	}
	if _, ok := s.GetInVolume("volA", "user/dir/b.txt"); !ok {
		t.Fatal("volA 不应受 volB 前缀清理影响")
	}
}
