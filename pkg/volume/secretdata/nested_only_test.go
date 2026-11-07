// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"path"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestNewFS_RejectsFlatLayout 钉住「**只支持嵌套布局**」（用户口径 2026-10-14：不支持旧格式目录、
// 无兼容）：把子容器搬到卷根（模拟旧扁平布局：容器都是卷根下的兄弟目录）后重挂载必须 **fail-closed**，
// 且错误指明「旧扁平布局」——绝不静默按扁平布局加载（那会让容器链差分/配额记账与空祖先回收静默失效）。
//
// 变异：去掉 validateNestedContainers 调用 ⇒ 本测红（旧布局被静默接受）。
func TestNewFS_RejectsFlatLayout(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	opts := Options{Secret: []byte("nested-only-key-0123"), MetaPadBytes: 64 << 10, TempDir: t.TempDir()}
	fs, err := NewFS(syncpkg.NewLocalFS(rootDir, nil), opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if merr := fs.MakeDir(ctx, "1/2"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	child := fs.dirSegs["1/2"]
	if child == "" {
		t.Fatal("子容器未登记")
	}
	// 把子容器搬到**卷根**（旧扁平布局的形态：所有容器都是卷根下的兄弟目录）。
	if rerr := fs.inner.Rename(ctx, child, path.Base(child)); rerr != nil {
		t.Fatalf("模拟扁平布局（搬容器到卷根）: %v", rerr)
	}
	_, merr := NewFS(syncpkg.NewLocalFS(rootDir, nil), opts)
	if merr == nil {
		t.Fatal("旧扁平布局必须 fail-closed 拒绝挂载（无兼容）")
	}
	if !strings.Contains(merr.Error(), "旧扁平布局") {
		t.Fatalf("错误应指明旧扁平布局: %v", merr)
	}
}
