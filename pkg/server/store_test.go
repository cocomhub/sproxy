// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- ChecksumStore 测试 ----

func TestChecksumStore_DeletePrefix(t *testing.T) {
	tmpDir := t.TempDir()
	cs := NewChecksumStore(tmpDir, nil)

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
	cs2 := NewChecksumStore(tmpDir, nil)
	if _, ok := cs2.Get("dir1/a.txt"); ok {
		t.Fatal("persisted file still has deleted prefix entry")
	}
	if v, ok := cs2.Get("root.txt"); !ok || v != "rrr" {
		t.Fatal("root.txt should persist")
	}
}

func TestChecksumStore_Rename_ToExisting(t *testing.T) {
	tmpDir := t.TempDir()
	cs := NewChecksumStore(tmpDir, nil)

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
	cs := NewChecksumStore(tmpDir, nil)
	cs.Set("k1", "v1")
	cs.Set("k2", "v2")

	// 新建实例从磁盘加载
	cs2 := NewChecksumStore(tmpDir, nil)
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
	cs := NewChecksumStore(tmpDir, nil)

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

// ---- UploadStore 测试 ----

func TestUploadStore_GetSessionByFilename(t *testing.T) {
	tmpDir := t.TempDir()
	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	us.CreateSession("id1", "file1.txt", 100, 4096, 1, strings.Repeat("a", 64), 0)
	us.CreateSession("id2", "file2.txt", 200, 4096, 1, strings.Repeat("b", 64), 0)

	s := us.GetSessionByFilename("file1.txt")
	if s == nil {
		t.Fatal("expected session for file1.txt")
	}
	if s.UploadID != "id1" {
		t.Fatalf("expected id1, got %s", s.UploadID)
	}

	if us.GetSessionByFilename("nonexistent.txt") != nil {
		t.Fatal("expected nil for nonexistent filename")
	}
}

func TestUploadStore_DeleteSession(t *testing.T) {
	tmpDir := t.TempDir()
	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	us.CreateSession("del-id", "del.txt", 100, 4096, 1, strings.Repeat("c", 64), 0)

	us.DeleteSession("del-id")

	if us.GetSession("del-id") != nil {
		t.Fatal("session should be nil after delete")
	}

	sessionDir := filepath.Join(tmpDir, ".__chunked__", "del-id")
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatal("session dir should be removed from disk")
	}
}

func TestUploadStore_CleanupExpired(t *testing.T) {
	tmpDir := t.TempDir()
	// Use a negative TTL so the session is already expired on creation
	us := NewUploadStore(tmpDir, -time.Nanosecond, nil)
	defer us.Stop()

	us.CreateSession("expired-id", "expired.txt", 100, 4096, 1, strings.Repeat("d", 64), 0)

	us.cleanupExpired()

	if us.GetSession("expired-id") != nil {
		t.Fatal("expired session should be cleaned up")
	}
}

func TestUploadStore_RecoverFromDisk(t *testing.T) {
	tmpDir := t.TempDir()

	us1 := NewUploadStore(tmpDir, 24*time.Hour, nil)
	us1.CreateSession("recover-id", "recover.txt", 8192, 4096, 2, strings.Repeat("e", 64), 0)
	us1.MarkChunkReceived("recover-id", 0, "chunk0hash")
	us1.Stop()

	us2 := NewUploadStore(tmpDir, 24*time.Hour, nil)
	defer us2.Stop()

	s := us2.GetSession("recover-id")
	if s == nil {
		t.Fatal("session should be recovered from disk")
	}
	if s.Filename != "recover.txt" {
		t.Fatalf("filename mismatch: %s", s.Filename)
	}
	if !s.ReceivedChunks[0] {
		t.Fatal("chunk 0 should be marked received after recovery")
	}
	if s.ReceivedChunks[1] {
		t.Fatal("chunk 1 should not be marked received")
	}
}

func TestUploadStore_ReconcileChunks(t *testing.T) {
	tmpDir := t.TempDir()

	us1 := NewUploadStore(tmpDir, 24*time.Hour, nil)
	us1.CreateSession("reconcile-id", "reconcile.txt", 8192, 4096, 2, strings.Repeat("f", 64), 0)
	us1.MarkChunkReceived("reconcile-id", 0, "chunk0hash")
	us1.Stop()

	// MarkChunkReceived only updates the bitmap, doesn't write chunk files.
	// Write a chunk file on disk to simulate a partial upload where the chunk
	// file exists but the bitmap wasn't updated (crash before bitmap flush).
	sessionDir := filepath.Join(tmpDir, ".__chunked__", "reconcile-id")
	chunkFile := filepath.Join(sessionDir, "00001.chunk")
	if err := os.WriteFile(chunkFile, []byte("fake chunk data"), 0644); err != nil {
		t.Fatalf("write chunk file: %v", err)
	}

	us2 := NewUploadStore(tmpDir, 24*time.Hour, nil)
	defer us2.Stop()

	s := us2.GetSession("reconcile-id")
	if s == nil {
		t.Fatal("session should be recovered")
	}

	// reconcileChunks should have detected the orphan chunk file on disk
	// and marked chunk index 1 as received, even though the bitmap from
	// session.json had it as false.
	if !s.ReceivedChunks[1] {
		t.Fatal("chunk 1 should be marked received after reconcile")
	}
}

func TestUploadStore_GetOrCreateSession_Reuse(t *testing.T) {
	tmpDir := t.TempDir()
	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	s1, reused, err := us.GetOrCreateSession("rid", "r.txt", 100, 4096, 1, strings.Repeat("g", 64), 0)
	if err != nil {
		t.Fatalf("first GetOrCreate: %v", err)
	}
	if reused {
		t.Fatal("first call should not be reused")
	}

	s2, reused, err := us.GetOrCreateSession("rid", "r.txt", 100, 4096, 1, strings.Repeat("g", 64), 0)
	if err != nil {
		t.Fatalf("second GetOrCreate: %v", err)
	}
	if !reused {
		t.Fatal("second call should reuse session")
	}
	if s1.UploadID != s2.UploadID {
		t.Fatal("upload_id should match")
	}
}

func TestUploadStore_ConcurrentMarkChunk(t *testing.T) {
	tmpDir := t.TempDir()
	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	const totalChunks = 100
	us.CreateSession("concurrent-id", "concurrent.txt", int64(totalChunks*4096), 4096, totalChunks, strings.Repeat("h", 64), 0)

	var wg sync.WaitGroup
	for i := range totalChunks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := us.MarkChunkReceived("concurrent-id", idx, fmt.Sprintf("chunk%dhash", idx)); err != nil {
				t.Errorf("MarkChunkReceived(%d): %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	if !us.AllChunksReceived("concurrent-id") {
		t.Fatal("all chunks should be received after concurrent marking")
	}
}

func TestUploadStore_CleanupSessionAfter(t *testing.T) {
	tmpDir := t.TempDir()
	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	sessionID := "cleanup-test"
	us.CreateSession(sessionID, "cleanup.txt", 1024, 256, 4, "abcd", 0)

	// 检查 session 存在
	if us.GetSession(sessionID) == nil {
		t.Fatal("expected session to exist")
	}

	// 计划在 50ms 后清理
	us.CleanupSessionAfter(sessionID, 50*time.Millisecond)

	// 验证 50ms 后 session 被移除
	time.Sleep(100 * time.Millisecond)

	if us.GetSession(sessionID) != nil {
		t.Error("expected session to be cleaned up after TTL")
	}
}
