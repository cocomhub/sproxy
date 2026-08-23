// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package buildmeta

import "testing"

// TestDirtyInfo_Export 确保 DirtyInfo/DirtyID 导出且 DirtyID 非空。
// embed 要求 internal/build/dirty_info.txt 文件存在才能编译通过；
// 该文件由 Makefile prepare 目标生成（git diff HEAD），本地/CI 先跑 make prepare。
func TestDirtyInfo_Export(t *testing.T) {
	if DirtyID() == "" {
		t.Error("DirtyID should not be empty")
	}
	if DirtyInfo() == "" {
		t.Log("DirtyInfo is empty (clean working tree)")
	}
}

// TestMd5hex10_Clean 覆盖 md5hex10 的空输入分支（embed 内容为空白时的 clean 语义）。
func TestMd5hex10_Clean(t *testing.T) {
	if got := md5hex10(""); got != "clean" {
		t.Errorf("md5hex10(\"\") = %q, want clean", got)
	}
}

// TestMd5hex10_Length 覆盖 md5hex10 的摘要长度（10 位 hex）。
func TestMd5hex10_Length(t *testing.T) {
	if got := md5hex10("some diff content"); len(got) != 10 {
		t.Errorf("md5hex10(len) = %q (len %d), want 10", got, len(got))
	}
}
