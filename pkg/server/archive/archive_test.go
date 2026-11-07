// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestAddFileToTar_Recursive 目录递归打包：条目名用 tarRel 前缀，内容与源一致。
func TestAddFileToTar_Recursive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if mkErr := os.MkdirAll(filepath.Join(dir, "user", "sub"), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(dir, "user", "sub", "a.txt"), []byte("hello"), 0o644); wErr != nil {
		t.Fatalf("write: %v", wErr)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := AddFileToTar(tw, root, "user", "pkg", slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("AddFileToTar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	tr := tar.NewReader(&buf)
	got := map[string]string{}
	for {
		h, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("next: %v", rerr)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if got["pkg/sub/a.txt"] != "hello" {
		t.Fatalf("tar 条目 = %v，want pkg/sub/a.txt=hello", got)
	}
}

// TestAddFileToTar_SymlinkSkipped 符号链接被跳过（不跟随、不逃逸）。
func TestAddFileToTar_SymlinkSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if mkErr := os.MkdirAll(filepath.Join(dir, "user"), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if wErr := os.WriteFile(outside, []byte("secret"), 0o644); wErr != nil {
		t.Fatalf("write outside: %v", wErr)
	}
	if lErr := os.Symlink(outside, filepath.Join(dir, "user", "link.txt")); lErr != nil {
		t.Skipf("symlink 不可用: %v", lErr)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := AddFileToTar(tw, root, "user", "pkg", slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("AddFileToTar: %v", err)
	}
	_ = tw.Close()

	tr := tar.NewReader(&buf)
	for {
		h, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("next: %v", rerr)
		}
		if h.Name == "pkg/link.txt" {
			t.Fatal("符号链接不应进入归档")
		}
	}
}
