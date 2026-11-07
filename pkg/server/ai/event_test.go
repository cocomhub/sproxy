// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// event_test.go 验证文件事件分类（IsFileAction）：显式文件动作 + file.* 前缀。

import "testing"

// TestIsFileAction 文件变更动作判定。
func TestIsFileAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		action string
		want   bool
	}{
		{action: "upload", want: true},
		{action: "delete", want: true},
		{action: "rename", want: true},
		{action: "mkdir", want: true},
		{action: "rmdir", want: true},
		{action: "version", want: true},
		{action: "restore", want: true},
		{action: "file.chmod", want: true},
		{action: "file.", want: true},
		{action: "chmod", want: false},
		{action: "files.upload", want: false},
		{action: "", want: false},
		{action: "share.create", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			t.Parallel()
			if got := IsFileAction(tc.action); got != tc.want {
				t.Fatalf("IsFileAction(%q) = %v, want %v", tc.action, got, tc.want)
			}
		})
	}
}
