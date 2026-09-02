// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"strings"
	"testing"
)

// TestValidSegmentName 段名校验表驱动（Windows 保留字/尾点/尾空格/大小写/分隔符/超长）。
func TestValidSegmentName(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"alice", true}, {"anonymous", true}, {"ak-abc123", true},
		{"", false}, {".", false}, {"..", false}, {"a/b", false}, {`a\b`, false},
		{"CON", false}, {"con", false}, {"CON.txt", false}, // 保留字含扩展名（基名判定）
		{"NUL", false}, {"PRN", false}, {"AUX", false},
		{"COM1", false}, {"lpt9", false}, {"COM10", true}, // COM10 合法
		{"foo.", false}, {"foo ", false}, // 尾点/尾空格
		{"a:b", false}, {`a<b`, false}, {`a>b`, false}, {`a"b`, false},
		{"a|b", false}, {"a?b", false}, {"a*b", false},
		{".__cloud__", false}, // 魔法前缀禁止
		{strings.Repeat("x", 256), false},
	}
	for _, c := range cases {
		if got := ValidSegmentName(c.in); got != c.ok {
			t.Fatalf("ValidSegmentName(%q)=%v want %v", c.in, got, c.ok)
		}
	}
}

// TestValidSegmentName_Extra 补充边界：255 长度合法、LPT10 合法、__ 前缀合法、扩展名、CJK。
func TestValidSegmentName_Extra(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{strings.Repeat("x", 255), true},
		{"LPT10", true},
		{"report.pdf", true},
		{"__version__", true}, // 段名校验允许 __ 前缀（UserRel 首段另有保留限制）
		{"_.tmp", true},
		{"文件名.txt", true}, // CJK 合法
		{"a b", true},     // 段内空格合法（仅首尾空格禁止）
		{" ", false},
		{".", false},
	}
	for _, c := range cases {
		if got := ValidSegmentName(c.in); got != c.ok {
			t.Errorf("ValidSegmentName(%q)=%v want %v", c.in, got, c.ok)
		}
	}
}
