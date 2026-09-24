// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestKeyValidate 钉住 key 段校验与 pkg/storage.ValidSegmentName 语义一致：
// 拒绝 .. / 绝对路径 / 空字节 / Windows 非法字符 / 保留设备名；
// 放行 checksum/<owner>/dir/f.txt（name 段含 "/"）。
func TestKeyValidate(t *testing.T) {
	t.Parallel()
	rejected := []string{
		"",
		"a",              // 单段（无类型分片）
		"../x/y",         // 路径穿越段
		"a/b/../c",       // 段含 ..
		"/abs/type/name", // 绝对路径
		"a/b\x00c/d",     // 空字节
		"a/b/c?d",        // Windows 非法字符
		"a/CON/b",        // 保留设备名
		"a/.__magic/b",   // 魔法前缀
		"a/b/c.",         // 尾点
		"a/b/c ",         // 尾空格
		"a//b",           // 空段
		"a/b/c\\d",       // 反斜杠
	}
	for _, key := range rejected {
		if _, err := validateKey(key); err == nil {
			t.Errorf("validateKey(%q) 应拒绝", key)
		}
	}
	accepted := []string{
		"checksum/alice/dir/f.txt", // name 段含 "/"（checksum rel 保留）
		"credential/anonymous/ring",
		"share/tok1",
		"audit/20260924",
		"quota/alice",
		"dedup/alice/abc123",
	}
	for _, key := range accepted {
		if _, err := validateKey(key); err != nil {
			t.Errorf("validateKey(%q) 应放行: %v", key, err)
		}
	}
	// 逐字对照 pkg/storage.ValidSegmentName：段判定一致（防语义漂移）。
	for _, seg := range []string{"alice", "dir", "f.txt", "ring", "tok1", "abc123", "..", ".", "CON", ".__x", "a/b"} {
		if got := validSegment(seg); got != storage.ValidSegmentName(seg) {
			t.Errorf("validSegment(%q) = %v, want storage.ValidSegmentName = %v（语义必须一致）", seg, got, storage.ValidSegmentName(seg))
		}
	}
}
