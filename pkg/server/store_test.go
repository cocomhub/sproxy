// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
)

// ---- ChecksumStore 测试 ----

func TestChecksumStore_DeletePrefix(t *testing.T) {
	tmpDir := t.TempDir()
	cs := checksum.NewChecksumStore(filepath.Join(tmpDir, "checksums.json"), nil)

	cs.Set("dir1/a.txt", "aaa")
	cs.Set("dir1/b.txt", "bbb")
	cs.Set("dir2/c.txt", "ccc")
	cs.Set("root.txt", "rrr")

	cs.DeletePrefix("dir1/")

	if _, ok := cs.Get("dir1/a.txt"); ok {
		t.Fatal("dir1/a.txt should be deleted")
	}
	if _, ok := cs.Get("dir1/b.txt"); ok {
		t.Fatal("dir1/b.txt should be deleted")
	}
	if _, ok := cs.Get("dir2/c.txt"); !ok {
		t.Fatal("dir2/c.txt should still exist")
	}
	if _, ok := cs.Get("root.txt"); !ok {
		t.Fatal("root.txt should still exist")
	}

	// 重新加载验证持久化
	cs2 := checksum.NewChecksumStore(filepath.Join(tmpDir, "checksums.json"), nil)
	if _, ok := cs2.Get("dir1/a.txt"); ok {
		t.Fatal("persisted file still has deleted prefix entry")
	}
	if v, ok := cs2.Get("root.txt"); !ok || v != "rrr" {
		t.Fatal("root.txt should persist")
	}
}

func TestChecksumStore_Rename_ToExisting(t *testing.T) {
	tmpDir := t.TempDir()
	cs := checksum.NewChecksumStore(filepath.Join(tmpDir, "checksums.json"), nil)

	cs.Set("from.txt", "fromVal")
	cs.Set("to.txt", "toVal")

	cs.Rename("from.txt", "to.txt")

	if _, ok := cs.Get("from.txt"); ok {
		t.Fatal("from.txt should be gone after rename")
	}
	v, ok := cs.Get("to.txt")
	if !ok || v != "fromVal" {
		t.Fatalf("to.txt should have fromVal, got %q", v)
	}
}

func TestChecksumStore_RecoverFromDisk(t *testing.T) {
	tmpDir := t.TempDir()
	cs := checksum.NewChecksumStore(filepath.Join(tmpDir, "checksums.json"), nil)
	cs.Set("k1", "v1")
	cs.Set("k2", "v2")

	// 新建实例从磁盘加载
	cs2 := checksum.NewChecksumStore(filepath.Join(tmpDir, "checksums.json"), nil)
	all := cs2.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	if all["k1"] != "v1" || all["k2"] != "v2" {
		t.Fatalf("content mismatch: %v", all)
	}
}

func TestChecksumStore_GetAll_Consistency(t *testing.T) {
	tmpDir := t.TempDir()
	cs := checksum.NewChecksumStore(filepath.Join(tmpDir, "checksums.json"), nil)

	for i := range 100 {
		cs.Set(fmt.Sprintf("f%d", i), fmt.Sprintf("cs%d", i))
	}
	for i := range 50 {
		cs.Delete(fmt.Sprintf("f%d", i))
	}

	all := cs.GetAll()
	if len(all) != 50 {
		t.Fatalf("expected 50 entries after delete, got %d", len(all))
	}
	for i := 50; i < 100; i++ {
		key := fmt.Sprintf("f%d", i)
		want := fmt.Sprintf("cs%d", i)
		if all[key] != want {
			t.Fatalf("key %s: want %s, got %s", key, want, all[key])
		}
	}
}
