// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testResumeState 是断点状态的最小可序列化结构（测试用）。
type testResumeState struct {
	UploadID string `json:"upload_id"`
	Progress int64  `json:"progress"`
}

func TestResume_Roundtrip(t *testing.T) {
	t.Parallel()
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	state := &testResumeState{UploadID: "u-123", Progress: 4096}
	if err := l.SaveResume("f.txt", state); err != nil {
		t.Fatalf("SaveResume: %v", err)
	}
	var got testResumeState
	if err := l.LoadResume("f.txt", &got); err != nil {
		t.Fatalf("LoadResume: %v", err)
	}
	if got.UploadID != state.UploadID || got.Progress != state.Progress {
		t.Fatalf("LoadResume = %+v, want %+v", got, *state)
	}
}

func TestResume_LoadMissing(t *testing.T) {
	t.Parallel()
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	var got testResumeState
	if err := l.LoadResume("nope.txt", &got); err == nil {
		t.Fatal("LoadResume 不存在时应返回错误")
	}
}

func TestResume_Delete(t *testing.T) {
	t.Parallel()
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	state := &testResumeState{UploadID: "u-1", Progress: 1}
	if err := l.SaveResume("f.txt", state); err != nil {
		t.Fatalf("SaveResume: %v", err)
	}
	if err := l.DeleteResume("f.txt"); err != nil {
		t.Fatalf("DeleteResume: %v", err)
	}
	var got testResumeState
	if err := l.LoadResume("f.txt", &got); err == nil {
		t.Fatal("DeleteResume 后 LoadResume 应失败")
	}
	if _, statErr := os.Stat(filepath.Join(l.Resume, "f.txt.json")); !os.IsNotExist(statErr) {
		t.Fatalf("resume 文件应已删除, stat err=%v", statErr)
	}
}

func TestResume_AtomicWrite_NoPartial(t *testing.T) {
	t.Parallel()
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	// 连续多次 SaveResume 不应产生残留 tmp 文件。
	for i := range 5 {
		if err := l.SaveResume("f.txt", &testResumeState{UploadID: "u", Progress: int64(i)}); err != nil {
			t.Fatalf("SaveResume #%d: %v", i, err)
		}
	}
	entries, readErr := os.ReadDir(l.Resume)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("resume 目录应有 1 个文件, got %d: %v", len(entries), entries)
	}
}

// TestResume_InvalidJSON 验证损坏文件返回错误而非 panic。
func TestResume_InvalidJSON(t *testing.T) {
	t.Parallel()
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	path := filepath.Join(l.Resume, "bad.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var got testResumeState
	if err := l.LoadResume("bad", &got); err == nil {
		t.Fatal("损坏 JSON 应返回错误")
	}
}

var _ = json.Marshal // 防误删 import（测试结构体已用 json tag）
