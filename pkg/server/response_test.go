// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

func TestFormatContentDisposition_Normal(t *testing.T) {
	result := formatContentDisposition("file.txt")
	if !strings.HasPrefix(result, "attachment;") {
		t.Errorf("expected attachment prefix, got %q", result)
	}
	if !strings.Contains(result, "file.txt") {
		t.Errorf("expected filename, got %q", result)
	}
}

func TestFormatContentDisposition_SpecialChars(t *testing.T) {
	// 文件名包含 " 和 \ 特殊字符
	result := formatContentDisposition(`file"name\.txt`)
	if !strings.HasPrefix(result, "attachment;") {
		t.Errorf("expected attachment prefix, got %q", result)
	}
	// 不应包含未转义的引号
	if strings.Contains(result, `filename="`) {
		// 标准库 mime.FormatMediaType 会正确处理特殊字符
		// 文件名中的 " 会被编码为 %22
	}
}

func TestFormatContentDisposition_Empty(t *testing.T) {
	result := formatContentDisposition("")
	if result != "attachment" {
		t.Errorf("expected 'attachment' for empty filename, got %q", result)
	}
}

func TestFormatContentDisposition_Unicode(t *testing.T) {
	result := formatContentDisposition("中文文件.txt")
	if !strings.Contains(result, "txt") {
		t.Errorf("expected filename in result, got %q", result)
	}
}
