// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockserver_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil/mockserver"
)

func TestMockChecksumStore_SetGet(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("file.txt", "abc123")
	got, ok := cs.Get("file.txt")
	if !ok || got != "abc123" {
		t.Fatalf("expected 'abc123', got %q (ok=%v)", got, ok)
	}
}

func TestMockChecksumStore_GetMissing(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	_, ok := cs.Get("missing.txt")
	if ok {
		t.Fatal("expected false for missing key")
	}
}

func TestMockChecksumStore_Delete(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("f.txt", "x")
	cs.Delete("f.txt")
	_, ok := cs.Get("f.txt")
	if ok {
		t.Fatal("expected false after delete")
	}
}

func TestMockChecksumStore_Rename(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("old", "cksum")
	cs.Rename("old", "new")
	_, okOld := cs.Get("old")
	v, okNew := cs.Get("new")
	if okOld || !okNew || v != "cksum" {
		t.Fatal("Rename failed")
	}
}

func TestMockChecksumStore_DeletePrefix(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("dir1/a.txt", "1")
	cs.Set("dir1/b.txt", "2")
	cs.Set("dir2/c.txt", "3")
	cs.DeletePrefix("dir1/")
	_, ok1 := cs.Get("dir1/a.txt")
	_, ok2 := cs.Get("dir1/b.txt")
	_, ok3 := cs.Get("dir2/c.txt")
	if ok1 || ok2 || !ok3 {
		t.Fatal("DeletePrefix failed: expected dir1/ entries gone, dir2/ preserved")
	}
}

func TestMockChecksumStore_GetAll(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("a", "1")
	cs.Set("b", "2")
	all := cs.GetAll()
	if len(all) != 2 || all["a"] != "1" || all["b"] != "2" {
		t.Fatal("GetAll returned unexpected data")
	}
}

func TestMockUploadStore_CreateAndGetSession(t *testing.T) {
	us := mockserver.NewUploadStore()
	s, err := us.CreateSession("sid1", "f.txt", 100, 64, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if s.Filename != "f.txt" {
		t.Fatalf("expected filename 'f.txt', got %q", s.Filename)
	}
	if s.TotalSize != 100 || s.ChunkSize != 64 || s.TotalChunks != 2 {
		t.Fatalf("unexpected session fields: totalSize=%d chunkSize=%d totalChunks=%d",
			s.TotalSize, s.ChunkSize, s.TotalChunks)
	}

	got := us.GetSession("sid1")
	if got == nil || got.UploadID != "sid1" {
		t.Fatal("GetSession failed")
	}
}

func TestMockUploadStore_CreateSessionDuplicate(t *testing.T) {
	us := mockserver.NewUploadStore()
	_, err := us.CreateSession("sid1", "f.txt", 100, 64, 2, "", 0)
	if err != nil {
		t.Fatalf("first CreateSession should succeed: %v", err)
	}
	_, err = us.CreateSession("sid1", "f2.txt", 200, 64, 3, "", 0)
	if err == nil {
		t.Fatal("expected error on duplicate session creation")
	}
}

func TestMockUploadStore_MarkAndCheckChunks(t *testing.T) {
	us := mockserver.NewUploadStore()
	_, err := us.CreateSession("sid1", "f.txt", 100, 64, 3, "", 0)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if us.AllChunksReceived("sid1") {
		t.Fatal("expected false when no chunks received")
	}

	if err := us.MarkChunkReceived("sid1", 0, "c1"); err != nil {
		t.Fatalf("MarkChunkReceived(0) failed: %v", err)
	}
	if us.AllChunksReceived("sid1") {
		t.Fatal("expected false when only 1/3 chunks received")
	}

	if err := us.MarkChunkReceived("sid1", 1, "c2"); err != nil {
		t.Fatalf("MarkChunkReceived(1) failed: %v", err)
	}
	if err := us.MarkChunkReceived("sid1", 2, "c3"); err != nil {
		t.Fatalf("MarkChunkReceived(2) failed: %v", err)
	}
	if !us.AllChunksReceived("sid1") {
		t.Fatal("expected true when all chunks received")
	}
}

func TestMockUploadStore_MarkChunkOutOfRange(t *testing.T) {
	us := mockserver.NewUploadStore()
	_, err := us.CreateSession("sid1", "f.txt", 100, 64, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	err = us.MarkChunkReceived("sid1", 99, "x")
	if err == nil {
		t.Fatal("expected error for out-of-range chunk index")
	}

	err = us.MarkChunkReceived("nonexistent", 0, "x")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestMockUploadStore_CompleteSession(t *testing.T) {
	us := mockserver.NewUploadStore()
	_, err := us.CreateSession("sid1", "f.txt", 100, 64, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if err := us.CompleteSession("sid1"); err != nil {
		t.Fatalf("CompleteSession failed: %v", err)
	}

	// Double-complete should fail
	if err := us.CompleteSession("sid1"); err == nil {
		t.Fatal("expected error on double complete")
	}

	// CompleteSession on nonexistent session should fail
	if err := us.CompleteSession("nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestMockUploadStore_GetOrCreateSession(t *testing.T) {
	us := mockserver.NewUploadStore()

	// Create new
	session, found, err := us.GetOrCreateSession("sid2", "g.txt", 200, 128, 4, "abc", 12345)
	if err != nil {
		t.Fatalf("GetOrCreateSession failed: %v", err)
	}
	if found {
		t.Fatal("expected found=false for new session")
	}
	if session.Filename != "g.txt" {
		t.Fatalf("expected filename 'g.txt', got %q", session.Filename)
	}

	// Get existing
	_, found, err = us.GetOrCreateSession("sid2", "g.txt", 200, 128, 4, "abc", 12345)
	if err != nil {
		t.Fatalf("GetOrCreateSession (existing) failed: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for existing session")
	}
}

func TestMockUploadStore_DeleteSession(t *testing.T) {
	us := mockserver.NewUploadStore()
	_, err := us.CreateSession("sid1", "f.txt", 100, 64, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	us.DeleteSession("sid1")
	if us.GetSession("sid1") != nil {
		t.Fatal("expected nil after DeleteSession")
	}
}

func TestMockUploadStore_GetSessionByFilename(t *testing.T) {
	us := mockserver.NewUploadStore()
	us.CreateSession("sid1", "a.txt", 100, 64, 2, "", 0)
	us.CreateSession("sid2", "b.txt", 200, 64, 2, "", 0)

	s := us.GetSessionByFilename("b.txt")
	if s == nil || s.UploadID != "sid2" {
		t.Fatal("GetSessionByFilename failed to find b.txt")
	}

	// Completed sessions should be excluded
	us.CompleteSession("sid1")
	s = us.GetSessionByFilename("a.txt")
	if s != nil {
		t.Fatal("GetSessionByFilename should not return completed session")
	}
}

func TestMockUploadStore_HealthAndStop(t *testing.T) {
	us := mockserver.NewUploadStore()
	if err := us.Health(); err != nil {
		t.Fatal("expected Health() to return nil")
	}
	// Stop should not panic
	us.Stop()
}
