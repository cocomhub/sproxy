// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestPrefixFS_KeySpaceMapping FS-CORE-3：备份目标键空间前缀映射——写入加 `<prefix>`，
// 读回（Stat.Path）剥前缀使引擎键空间与源一致。
func TestPrefixFS_KeySpaceMapping(t *testing.T) {
	t.Parallel()
	inner := &extFS{files: map[string]string{}}
	p := &prefixFS{FS: inner, prefix: "alice/user/"}
	ctx := context.Background()
	if err := p.WriteFile(ctx, "docs/a.txt", strings.NewReader("hi"), 2, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if inner.files["alice/user/docs/a.txt"] != "hi" {
		t.Fatalf("写入键应加前缀, got %v", inner.files)
	}
	e, err := p.Stat(ctx, "docs/a.txt")
	if err != nil || e == nil || e.Path != "docs/a.txt" || e.Size != 2 {
		t.Fatalf("Stat 应剥前缀返回调用方键: %+v err=%v", e, err)
	}
	// Inner 透传（供 Innermost 下探）。
	if p.Inner() == nil {
		t.Fatal("Inner 应返回被包装 FS")
	}
}

// TestNewExternalSource_SkipVerifyDisablesFileMeta skip_verify 对外部卷下载同口径生效
// （此前仅本地卷/转存消费该开关）。
func TestNewExternalSource_SkipVerifyDisablesFileMeta(t *testing.T) {
	t.Parallel()
	e := &syncpkg.Entry{Size: 1}
	s := newExternalSource(&extFS{files: map[string]string{}}, "k", e, true)
	if s.fileMeta != nil {
		t.Fatal("skip_verify=true 应禁用 FileMeta 注入")
	}
	s2 := newExternalSource(&extFS{files: map[string]string{}}, "k", e, false)
	if s2 == nil {
		t.Fatal("非 skip 应返回 source")
	}
}
