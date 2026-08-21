// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupDownloadItemsTest 创建带上传/下载 mock 的服务端，并在其上预置两个文件。
func setupDownloadItemsTest(t *testing.T) *FileClient {
	t.Helper()
	ts, dir := newMockServer(t)
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	// 预置两个文件供下载
	for _, name := range []string{"a.txt", "b.txt"} {
		content := []byte("content-" + name)
		if err := os.WriteFile(filepath.Join(dir, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// TestDownloadItems_Sequential 验证顺序下载多个文件全部成功。
func TestDownloadItems_Sequential(t *testing.T) {
	c := setupDownloadItemsTest(t)
	outDir := t.TempDir()

	items := []DownloadItem{
		{RemotePath: "a.txt", LocalPath: filepath.Join(outDir, "a.txt")},
		{RemotePath: "b.txt", LocalPath: filepath.Join(outDir, "b.txt")},
	}
	if err := c.DownloadItems(t.Context(), items, WithDownloadConcurrency(1)); err != nil {
		t.Fatalf("DownloadItems sequential failed: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		data, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("expected %s downloaded: %v", name, err)
		}
		if string(data) != "content-"+name {
			t.Fatalf("expected content for %s, got %q", name, data)
		}
	}
}

// TestDownloadItems_AggregatesErrors 验证部分文件失败时错误聚合返回，其余仍成功。
func TestDownloadItems_AggregatesErrors(t *testing.T) {
	c := setupDownloadItemsTest(t)
	outDir := t.TempDir()

	// a.txt 存在、missing.txt 不存在 → 一个成功一个失败，errors.Join 聚合
	items := []DownloadItem{
		{RemotePath: "a.txt", LocalPath: filepath.Join(outDir, "a.txt")},
		{RemotePath: "missing.txt", LocalPath: filepath.Join(outDir, "missing.txt")},
	}
	err := c.DownloadItems(t.Context(), items, WithDownloadConcurrency(2))
	if err == nil {
		t.Fatal("expected error when one item fails")
	}
	if !strings.Contains(err.Error(), "missing.txt") {
		t.Fatalf("expected error to mention missing.txt, got: %v", err)
	}
	// 成功项应已落盘（单文件失败不影响其余）
	if _, statErr := os.Stat(filepath.Join(outDir, "a.txt")); statErr != nil {
		t.Fatalf("expected a.txt downloaded despite partial failure: %v", statErr)
	}
}

// TestDownloadItems_EmptyRemotePath 验证 RemotePath 为空时记为错误（不 panic、不取消其余）。
func TestDownloadItems_EmptyRemotePath(t *testing.T) {
	c := setupDownloadItemsTest(t)
	outDir := t.TempDir()

	items := []DownloadItem{
		{RemotePath: "", LocalPath: filepath.Join(outDir, "empty.txt")},
		{RemotePath: "a.txt", LocalPath: filepath.Join(outDir, "a.txt")},
	}
	err := c.DownloadItems(t.Context(), items)
	if err == nil {
		t.Fatal("expected error when RemotePath is empty")
	}
	if !strings.Contains(err.Error(), "远程路径为空") {
		t.Fatalf("expected error mentioning empty path, got: %v", err)
	}
	// 非空项仍应成功
	if _, statErr := os.Stat(filepath.Join(outDir, "a.txt")); statErr != nil {
		t.Fatalf("expected a.txt downloaded despite empty path item: %v", statErr)
	}
}

// TestDownloadItems_LocalPathDefaultsToBasename 验证 LocalPath 为空时用 RemotePath 的 basename。
func TestDownloadItems_LocalPathDefaultsToBasename(t *testing.T) {
	c := setupDownloadItemsTest(t)
	outDir := t.TempDir()
	// t.Chdir 自动恢复原工作目录（Go 1.24+），使 basename 落盘于 outDir
	t.Chdir(outDir)

	items := []DownloadItem{{RemotePath: "a.txt"}}
	if err := c.DownloadItems(t.Context(), items); err != nil {
		t.Fatalf("DownloadItems failed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "a.txt")); statErr != nil {
		t.Fatalf("expected a.txt saved as basename: %v", statErr)
	}
}

// TestDownloadItems_ConcurrencyLimit 验证并发上限生效且结果正确。
func TestDownloadItems_ConcurrencyLimit(t *testing.T) {
	c := setupDownloadItemsTest(t)
	outDir := t.TempDir()

	items := []DownloadItem{
		{RemotePath: "a.txt", LocalPath: filepath.Join(outDir, "a.txt")},
		{RemotePath: "b.txt", LocalPath: filepath.Join(outDir, "b.txt")},
	}
	// 默认并发 2（不限制）
	if err := c.DownloadItems(t.Context(), items); err != nil {
		t.Fatalf("DownloadItems default failed: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, statErr := os.Stat(filepath.Join(outDir, name)); statErr != nil {
			t.Fatalf("expected %s downloaded: %v", name, statErr)
		}
	}
	// 空输入 → nil
	if err := c.DownloadItems(context.Background(), nil); err != nil {
		t.Fatalf("expected nil error for empty items, got: %v", err)
	}
}
