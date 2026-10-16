// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileSink_AppendRecent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewScopeSink(dir, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Append(Row{Type: TypeDownload, Step: "download", DurMS: 5}); err != nil {
		t.Fatal(err)
	}
	if err = s.Append(Row{Type: TypeError, Level: LevelError, Step: "encrypt", Err: "x"}); err != nil {
		t.Fatal(err)
	}
	rows := s.Recent(Filter{})
	if len(rows) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(rows), rows)
	}
	if f := s.Recent(Filter{Type: TypeError}); len(f) != 1 {
		t.Fatalf("filter type: %+v", f)
	}
	// 落盘格式：独立文件每行 JSON
	raw, err := os.ReadFile(filepath.Join(dir, "task-1.audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "\n"); got != 2 {
		t.Fatalf("want 2 lines, got %d", got)
	}
}

func TestFileSink_MultiTaskIsolation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := NewScopeSink(dir, "task-a")
	b, _ := NewScopeSink(dir, "task-b")
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	_ = a.Append(Row{Type: TypeDownload})
	_ = b.Append(Row{Type: TypeTransfer})
	fa := a.Recent(Filter{})
	fb := b.Recent(Filter{})
	if len(fa) != 1 || fa[0].Type != TypeDownload {
		t.Fatalf("task-a polluted: %+v", fa)
	}
	if len(fb) != 1 || fb[0].Type != TypeTransfer {
		t.Fatalf("task-b polluted: %+v", fb)
	}
}

func TestScopeSink_FilePerm0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0600 仅 Unix")
	}
	t.Parallel()
	dir := t.TempDir()
	s, _ := NewScopeSink(dir, "task-1")
	t.Cleanup(func() { _ = s.Close() })
	_ = s.Append(Row{Type: TypeDownload})
	fi, err := os.Stat(filepath.Join(dir, "task-1.audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm want 0600, got %v", fi.Mode().Perm())
	}
}
