// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// crosscheck_hotpath_test.go 钉住 P5 第二片：`crossCheckExternalChecksum` 走**轻量路径**
// （`meta.ChecksumProvider`），从而**不加载冷结构 meta**（实测单文件常驻 78 MB / 解析 ≈584 ms），
// 且**行为与走完整 `FileMeta` 时一致**。
//
// 变异：把实现改回 `pv.FileMeta(...)` ⇒ 用例 1 红。

import (
	"context"
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// hotCheckFS 只实现**轻量能力**（ChecksumAndSize）——若调用方仍走 FileMeta，则拿到空值 ⇒ 判据失效。
type hotCheckFS struct {
	syncpkg.FS
	ck string
}

func (h *hotCheckFS) ChecksumAndSize(_ context.Context, _ string) (string, int64, error) {
	return h.ck, 123, nil
}

// legacyCheckFS 只实现**完整 FileMeta**（无轻量能力）⇒ 验证回退路径仍工作。
type legacyCheckFS struct {
	syncpkg.FS
	total string
}

func (l *legacyCheckFS) FileMeta(_ context.Context, _ string) (*meta.FileMeta, error) {
	return &meta.FileMeta{TotalSHA256: l.total, Size: 99}, nil
}

// bareFS 两者都不实现 ⇒ 跳过判据（与原逻辑一致）。
type bareFS struct{ syncpkg.FS }

func TestCrossCheckExternalChecksum_UsesHotPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// 轻量路径给出的是**权威值** ⇒ 与它不一致时必须 409（证明真的用了轻量值）。
	hot := &hotCheckFS{ck: "hot-sha"}
	if err := crossCheckExternalChecksum(ctx, hot, "k", "hot-sha"); err != nil {
		t.Fatalf("一致时应放行: %v", err)
	}
	err := crossCheckExternalChecksum(ctx, hot, "k", "other-sha")
	if err == nil {
		t.Fatal("不一致时不得放行（否则 CAS 校验形同虚设）")
	}
	var he *files.HTTPError
	if !errors.As(err, &he) || he.Status != 409 {
		t.Fatalf("应为 409 HTTPError，得到 %v", err)
	}
}

func TestCrossCheckExternalChecksum_FallsBackToFileMeta(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacy := &legacyCheckFS{total: "legacy-sha"}
	if err := crossCheckExternalChecksum(ctx, legacy, "k", "legacy-sha"); err != nil {
		t.Fatalf("回退路径一致时应放行: %v", err)
	}
	if err := crossCheckExternalChecksum(ctx, legacy, "k", "mismatch"); err == nil {
		t.Fatal("回退路径不一致时必须 409")
	}
}

func TestCrossCheckExternalChecksum_SkipsWhenNoProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// 无 meta 载体 ⇒ 跳过判据（两种实现都没有）。
	if err := crossCheckExternalChecksum(ctx, &bareFS{}, "k", "whatever"); err != nil {
		t.Fatalf("无 meta 载体应跳过判据: %v", err)
	}
	// expected 为空 ⇒ 直接跳过。
	if err := crossCheckExternalChecksum(ctx, &hotCheckFS{ck: "x"}, "k", ""); err != nil {
		t.Fatalf("expected 为空应跳过: %v", err)
	}
}
