// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/i18n"
	"github.com/cocomhub/sproxy/pkg/client"
)

// TestTextFormatter_PrintShareList_English：LANG=en 下分享列表输出英文文案。
// 依赖 i18n 的环境探测（lookupEnv seam 由 i18n 包内测试覆盖）。
func TestTextFormatter_PrintShareList_English(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	// i18n.Locale 按 env 探测并缓存；本用例与并行用例无共享状态（env 隔离 + 包级
	// 缓存仅在同一个测试进程内被本包测试并发读——见 TestEnglishGolden 注释）。

	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareList([]client.ShareLink{
		{Token: "tok123", Filename: "file.txt", Downloads: 5, MaxDownloads: 10},
	})
	output := buf.String()
	if !strings.Contains(output, "active") {
		t.Fatalf("expected English 'active', got %q", output)
	}
	if !strings.Contains(output, "TOKEN") {
		t.Fatalf("expected header 'TOKEN', got %q", output)
	}
}

// TestTextFormatter_PrintShareList_Empty_English：LANG=en 下空列表输出英文占位文案。
func TestTextFormatter_PrintShareList_Empty_English(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")

	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareList(nil)
	output := buf.String()
	if !strings.Contains(output, "No share links") {
		t.Fatalf("expected English 'No share links', got %q", output)
	}
}

// TestTextFormatter_PrintStat_English：LANG=en 下 stat 输出英文词。
func TestTextFormatter_PrintStat_English(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")

	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintStat(&client.FileInfo{Name: "test.txt", Size: 42, Checksum: "abc123"}, "test.txt")
	output := buf.String()
	if !strings.Contains(output, "bytes") {
		t.Fatalf("expected English 'bytes', got %q", output)
	}
	if !strings.Contains(output, "name:") {
		t.Fatalf("expected 'name:' key, got %q", output)
	}
}

// TestTextFormatter_PrintStats_English：LANG=en 下统计输出英文词。
func TestTextFormatter_PrintStats_English(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")

	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintStats(&client.StatsResponse{
		DiskUsage: struct {
			StorageRoot string `json:"storage_root"`
			TotalFiles  int    `json:"total_files"`
			TotalSize   int64  `json:"total_size"`
		}{StorageRoot: "/data", TotalFiles: 10, TotalSize: 1000},
		RequestCounts: struct {
			Total     int64 `json:"total"`
			Status2xx int64 `json:"2xx"`
			Status4xx int64 `json:"4xx"`
			Status5xx int64 `json:"5xx"`
		}{},
	})
	output := buf.String()
	if !strings.Contains(output, "Server statistics") {
		t.Fatalf("expected English 'Server statistics', got %q", output)
	}
}

// TestEnglishGolden 锁定 LANG=en 下关键英文词条（golden 断言防词条漂移）。
func TestEnglishGolden(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	for key, want := range map[string]string{
		"暂无分享链接":       "No share links",
		"分享链接: %s":     "Share link: %s",
		"服务器统计（自启动以来）": "Server statistics (since startup)",
		"已设置":          "Set",
		"未设置":          "Not set",
	} {
		if got := i18n.T(key); got != want {
			t.Errorf("English dict[%q] = %q, want %q", key, got, want)
		}
	}
}
