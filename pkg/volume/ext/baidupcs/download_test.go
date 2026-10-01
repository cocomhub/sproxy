// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDownload_Resume 验证断点文件路径落在 Layout.Tmp 下（用户硬约束：中间态只依赖本地 FS）。
// downloadViaDownloader 需要真实 pcs 获取直链，此处验证布局与断点文件命名契约。
func TestDownload_Resume_Path(t *testing.T) {
	t.Parallel()
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	key := layout.SanitizeKey("/baidu/dir/f.txt")
	if key == "" {
		t.Fatal("SanitizeKey 不应为空")
	}
	// 断点文件应在 Layout.Tmp 下（<tmp>/<key>.download）。
	resumePath := filepath.Join(layout.Tmp, key+".download")
	if filepath.Dir(resumePath) != layout.Tmp {
		t.Fatalf("断点路径不在 Tmp 下: %q", resumePath)
	}
	if _, statErr := os.Stat(layout.Tmp); statErr != nil {
		t.Fatalf("Tmp 目录不存在: %v", statErr)
	}
}

// TestDownload_NoClient 库兜底下载无 client 时返回错误（而非 panic）。
func TestDownload_NoClient(t *testing.T) {
	t.Parallel()
	a := newLibraryAdapter(nil, testLogger())
	if err := a.Download(t.Context(), "/baidu/f.txt", filepath.Join(t.TempDir(), "f.txt")); err == nil {
		t.Fatal("无 client 下载应报错")
	}
}
