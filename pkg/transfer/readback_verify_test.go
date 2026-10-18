// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

// readback_verify_test.go 钉住「转存写后读回校验」在内核层生效（独立审核 P2 收敛，2026-10-18）。
//
// 缺陷：内核（本包）此前只查「目标存在且非空」⇒ 存储层静默截断/损坏被当成功（归档/通用转存
// 路径无内容校验，而 pkg/cloud 的下载转存路径有）。修法：把读回校验收敛为唯一实现
// （readback.go）并在 Execute 的单一出口调用 ⇒ 内容与目标卷 meta 不一致时**删除产物 + 失败**。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// corruptingMetaFS：写入真实落到 LocalFS，但 FileMeta 恒返回**正确内容**的 meta（模拟
// 写侧/存储层静默损坏：落盘字节与 meta 不符）。
type corruptingMetaFS struct {
	*syncpkg.LocalFS
	good *meta.FileMeta
}

func (f *corruptingMetaFS) FileMeta(_ context.Context, _ string) (*meta.FileMeta, error) {
	return f.good, nil
}

func goodMeta(t *testing.T, content string) *meta.FileMeta {
	t.Helper()
	c, err := meta.NewCalculator(int64(len(content)), int64(len(content)))
	if err != nil {
		t.Fatalf("NewCalculator: %v", err)
	}
	if _, werr := c.Write([]byte(content)); werr != nil {
		t.Fatalf("Calculator.Write: %v", werr)
	}
	return c.Finish()
}

func TestExecute_ReadbackMismatchDeletesTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	const goodContent = "GOOD-CONTENT-1234"
	fs := &corruptingMetaFS{LocalFS: inner, good: goodMeta(t, goodContent)}

	// 源内容**损坏**（与 meta 不符）⇒ 必须失败 + 删除产物。
	const bad = "CORRUPTED-CONTENT"
	_, err := Execute(ctx, TransferRequest{
		Entries:     []SourceEntry{{Rel: "src.bin", Size: int64(len(bad)), Reader: strings.NewReader(bad)}},
		Destination: fs,
		Scheme:      "sproxy",
		Volume:      "vol",
		Rel:         "dst.bin",
	})
	if err == nil {
		t.Fatal("内容与目标卷 meta 不一致必须失败（未修必红：内核只查「存在且非空」⇒ 静默成功）")
	}
	if !errors.Is(err, ErrReadbackMismatch) {
		t.Fatalf("应为 ErrReadbackMismatch，实际 %v", err)
	}
	// fail-closed：不得残留损坏对象。
	if e, serr := inner.Stat(ctx, "dst.bin"); serr == nil && e != nil {
		t.Fatalf("读回不一致后产物必须删除（未修必红：损坏对象残留）: %+v", e)
	}
}

// 对照组：内容与 meta 一致 ⇒ 成功且产物保留（防止"恒失败"式假绿）。
func TestExecute_ReadbackMatchSucceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	const content = "GOOD-CONTENT-1234"
	fs := &corruptingMetaFS{LocalFS: inner, good: goodMeta(t, content)}

	url, err := Execute(ctx, TransferRequest{
		Entries:     []SourceEntry{{Rel: "src.bin", Size: int64(len(content)), Reader: strings.NewReader(content)}},
		Destination: fs,
		Scheme:      "sproxy",
		Volume:      "vol",
		Rel:         "dst.bin",
	})
	if err != nil {
		t.Fatalf("内容与 meta 一致应成功，实际 %v", err)
	}
	if url == "" {
		t.Fatal("成功时必须返回目标 URL")
	}
	if e, serr := inner.Stat(ctx, "dst.bin"); serr != nil || e == nil {
		t.Fatalf("成功时产物必须保留（err=%v）", serr)
	}
}
