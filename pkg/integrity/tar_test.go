// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/integrity"
)

// writeTarGz 写一个含 entries（name→content）条目的 gzip 压缩 tar 到临时文件。
func writeTarGz(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// writeTar 写一个未压缩的 tar 到临时文件（含指定条目；空条目 map = 无条目空 tar）。
func writeTar(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "a.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestCheckTar_ValidGz：合法 tar.gz（含条目）应 OK。
func TestCheckTar_ValidGz(t *testing.T) {
	t.Parallel()
	path := writeTarGz(t, map[string]string{"a.txt": "x"})
	rep, err := integrity.TarChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("合法 tar.gz 不应返回 error，got %v", err)
	}
	if !rep.OK {
		t.Fatalf("合法 tar.gz 应 OK，got Reason=%q", rep.Reason)
	}
}

// TestCheckTar_ValidPlain：未压缩 .tar（含条目）应 OK（gzip 包装可选路径）。
func TestCheckTar_ValidPlain(t *testing.T) {
	t.Parallel()
	path := writeTar(t, map[string]string{"a.txt": "x"})
	rep, err := integrity.TarChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("合法 tar 不应返回 error，got %v", err)
	}
	if !rep.OK {
		t.Fatalf("合法 tar 应 OK，got Reason=%q", rep.Reason)
	}
}

// TestCheckTar_Corrupt：非 tar 字节应失败（不回 error，仅 OK=false）。
func TestCheckTar_Corrupt(t *testing.T) {
	t.Parallel()
	path := writeBytes(t, []byte("not-a-tar"))
	rep, err := integrity.TarChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("损坏 tar 应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("损坏 tar 应失败")
	}
}

// TestCheckTar_Empty：无条目 tar 应失败（Review Focus 2）。
func TestCheckTar_Empty(t *testing.T) {
	t.Parallel()
	path := writeTar(t, map[string]string{})
	rep, err := integrity.TarChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("空 tar 应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("空 tar 应失败（无条目）")
	}
}

// TestCheckTar_CorruptGz：gzip 头损坏的 tar.gz 应失败。
func TestCheckTar_CorruptGz(t *testing.T) {
	t.Parallel()
	path := writeBytes(t, []byte{0x1f, 0x8b, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff})
	rep, err := integrity.TarChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("损坏 gzip 应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("损坏 gzip tar 应失败")
	}
}

// TestTarChecker_Matches：扩展名族匹配（设计 §77 规则）。
func TestTarChecker_Matches(t *testing.T) {
	t.Parallel()
	c := integrity.TarChecker{}
	// R1-I2：zst/br 无解压器（未引入外部依赖），不匹配（避免误报 damaged）。
	for _, name := range []string{"a.tar", "b.tar.gz", "c.tgz", "f.TAR.GZ"} {
		if !c.Matches(name) {
			t.Errorf("Matches(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"x.txt", "y.png", "z.tar.xz"} {
		if c.Matches(name) {
			t.Errorf("Matches(%q) = true, want false", name)
		}
	}
}

// TestTarChecker_Kind：类型标识为注册键。
func TestTarChecker_Kind(t *testing.T) {
	t.Parallel()
	if (integrity.TarChecker{}).Kind() != "archive/tar" {
		t.Fatal("Kind 应为 archive/tar")
	}
}
