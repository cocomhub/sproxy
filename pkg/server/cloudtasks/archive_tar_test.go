// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

// archive_tar_test.go 钉住归档 tar.gz 打包助手（P1-c S4 迁入本包）的**落盘契约**：
// 返回的 checksum 必须等于落盘产物的 SHA-256（收尾顺序修复的回归锁）、产物必须可解压、
// 多条目的条目名/内容不变，以及 O_EXCL 冲突必须返回 errArchiveExists 且不破坏已有产物。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// newArchiveTARRoot 建一个临时 storage.Root（Windows 上先关句柄再清理目录）。
func newArchiveTARRoot(t *testing.T) (*storage.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// writeArchiveTARSource 在 root 内写一个源文件（自动建中间目录）。
func writeArchiveTARSource(t *testing.T, root *storage.Root, rel string, payload []byte) {
	t.Helper()
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open src %q: %v", rel, err)
	}
	if _, wErr := f.Write(payload); wErr != nil {
		_ = f.Close()
		t.Fatalf("write src %q: %v", rel, wErr)
	}
	if cErr := f.Close(); cErr != nil {
		t.Fatalf("close src %q: %v", rel, cErr)
	}
}

// tarEntries 解压 tar.gz 并把条目名映射到内容（同时验证 gzip 尾/tar 结构完整）。
func tarEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip 头解析失败（归档不完整）: %v", err)
	}
	tr := tar.NewReader(zr)
	entries := map[string]string{}
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("tar 解析失败（归档截断）: %v", nextErr)
		}
		body, readErr := io.ReadAll(tr)
		if readErr != nil {
			t.Fatalf("读 tar 条目 %q: %v", hdr.Name, readErr)
		}
		entries[hdr.Name] = string(body)
	}
	if cErr := zr.Close(); cErr != nil {
		t.Fatalf("gzip 尾校验失败（归档截断）: %v", cErr)
	}
	return entries
}

// TestCreateTarGz_ChecksumMatchesArtifact 归档 checksum 必须等于落盘文件的 SHA-256（且可解压）。
// 直接打 `createTarGz`（落桶路径的唯一实现），避免依赖下载端点/桶布局。
func TestCreateTarGz_ChecksumMatchesArtifact(t *testing.T) {
	t.Parallel()
	srcRoot, _ := newArchiveTARRoot(t)
	payload := []byte(strings.Repeat("payload-", 500))
	writeArchiveTARSource(t, srcRoot, "a.bin", payload)

	outRoot, outDir := newArchiveTARRoot(t)
	sum, err := createTarGz(srcRoot, "a.bin", "a.bin", outRoot, "check.tar.gz", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("createTarGz: %v", err)
	}
	raw, readErr := os.ReadFile(filepath.Join(outDir, "check.tar.gz"))
	if readErr != nil {
		t.Fatalf("读归档: %v", readErr)
	}

	// ① checksum 必须等于落盘产物的 SHA-256（收尾顺序修复前漏算 tar EOF + gzip 尾 ⇒ 恒不等）。
	want := sha256.Sum256(raw)
	if sum != hex.EncodeToString(want[:]) {
		t.Fatalf("checksum 与产物不符: 返回 %s, 实际 %s", sum, hex.EncodeToString(want[:]))
	}

	// ② 归档必须完整可解压，条目名 = tar 条目名入参、内容 = 源文件。
	entries := tarEntries(t, raw)
	if len(entries) != 1 {
		t.Fatalf("归档条目 = %v，期望仅 a.bin", entries)
	}
	if got := entries["a.bin"]; got != string(payload) {
		t.Fatalf("条目 a.bin 内容长度 = %d，期望 %d", len(got), len(payload))
	}
}

// TestCreateMultiFileTarGz_EntriesAndExists 锁多文件打包：条目名逐条按 ArchiveSource.TarRel、
// 内容按各自 Root/Rel 读取（跨租户根），checksum 与落盘一致；同名再打包返回 errArchiveExists
// 且不覆盖已有产物。
func TestCreateMultiFileTarGz_EntriesAndExists(t *testing.T) {
	t.Parallel()
	rootA, _ := newArchiveTARRoot(t)
	rootB, _ := newArchiveTARRoot(t)
	writeArchiveTARSource(t, rootA, "cloud/t1/a.bin", []byte("aaaaa"))
	writeArchiveTARSource(t, rootB, "cloud/t2/b.bin", []byte("bbbbb"))

	outRoot, outDir := newArchiveTARRoot(t)
	sources := []ArchiveSource{
		{Root: rootA, Rel: "cloud/t1/a.bin", TarRel: "t1/a.bin"},
		{Root: rootB, Rel: "cloud/t2/b.bin", TarRel: "t2/b.bin"},
	}
	logger := slog.New(slog.DiscardHandler)
	sum, err := createMultiFileTarGz(sources, outRoot, "multi.tar.gz", logger)
	if err != nil {
		t.Fatalf("createMultiFileTarGz: %v", err)
	}
	raw, readErr := os.ReadFile(filepath.Join(outDir, "multi.tar.gz"))
	if readErr != nil {
		t.Fatalf("读归档: %v", readErr)
	}
	want := sha256.Sum256(raw)
	if sum != hex.EncodeToString(want[:]) {
		t.Fatalf("checksum 与产物不符: 返回 %s, 实际 %s", sum, hex.EncodeToString(want[:]))
	}
	entries := tarEntries(t, raw)
	if len(entries) != 2 || entries["t1/a.bin"] != "aaaaa" || entries["t2/b.bin"] != "bbbbb" {
		t.Fatalf("归档条目 = %v，期望 t1/a.bin=aaaaa 与 t2/b.bin=bbbbb", entries)
	}

	// O_EXCL：同名再打包必须报 errArchiveExists，且原产物字节不变。
	if _, againErr := createMultiFileTarGz(sources, outRoot, "multi.tar.gz", logger); !errors.Is(againErr, errArchiveExists) {
		t.Fatalf("同名再打包 err = %v，期望 errArchiveExists", againErr)
	}
	after, readErr := os.ReadFile(filepath.Join(outDir, "multi.tar.gz"))
	if readErr != nil {
		t.Fatalf("读归档（冲突后）: %v", readErr)
	}
	if !bytes.Equal(after, raw) {
		t.Fatal("O_EXCL 冲突后已有归档被改写")
	}
}
