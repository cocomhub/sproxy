// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import "testing"

// 请求边界校验的用例：`validateCloudDownloadURL` 属**装配层的请求校验**（与 pkg/files 的
// 「路径解析留在装配层」同一分工——它只服务 HTTP 入口，领域侧只接收已清洗的 URL/文件名）。
// 云任务管理器迁入 pkg/cloud（S4-B）后，这些用例随之留在装配层。
//
// 用例名与迁移前完全一致（核对 ② 只禁丢失，允许换宿主文件）。

func TestValidateCloudDownloadURL_Valid(t *testing.T) {
	url, filename, err := validateCloudDownloadURL("https://example.com/file.zip", "", false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if url != "https://example.com/file.zip" {
		t.Fatalf("expected URL unchanged, got %q", url)
	}
	if filename != "file.zip" {
		t.Fatalf("expected extracted filename 'file.zip', got %q", filename)
	}
}

func TestValidateCloudDownloadURL_WithFilename(t *testing.T) {
	url, filename, err := validateCloudDownloadURL("https://example.com/data.bin", "custom.dat", false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if filename != "custom.dat" {
		t.Fatalf("expected 'custom.dat', got %q", filename)
	}
	if url != "https://example.com/data.bin" {
		t.Fatalf("expected URL unchanged, got %q", url)
	}
}

func TestValidateCloudDownloadURL_EmptyURL(t *testing.T) {
	_, _, err := validateCloudDownloadURL("", "", false)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestValidateCloudDownloadURL_InvalidScheme(t *testing.T) {
	_, _, err := validateCloudDownloadURL("ftp://example.com/file.zip", "", false)
	if err == nil {
		t.Fatal("expected error for ftp URL")
	}
}

func TestValidateCloudDownloadURL_PathTraversal(t *testing.T) {
	_, _, err := validateCloudDownloadURL("https://example.com/file.zip", "../../../etc/passwd", false)
	if err == nil {
		t.Fatal("expected error for unsafe filename")
	}
}

func TestValidateCloudDownloadURL_NoHost(t *testing.T) {
	_, _, err := validateCloudDownloadURL("not-a-url", "", false)
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestValidateCloudDownloadURL_QueryString(t *testing.T) {
	_, filename, err := validateCloudDownloadURL("https://example.com/download?file=test.zip&token=abc", "", false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// 新行为：查询参数附加在文件名后，经过 cloudfilename.Safe 后 ? 和 = 被替换为 _
	// 查询参数中的 = 和 & 在文件名中合法（多数系统允许），Safe 保留它们
	if filename != "download_file=test.zip&token=abc" {
		t.Fatalf("expected extracted filename 'download_file=test.zip&token=abc', got %q", filename)
	}
}
