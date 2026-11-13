// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// checksum_forward_test.go 钉住 R21 对**新可选能力** `meta.ChecksumProvider` 的转发：
// 装饰器不转发 ⇒ 调用方 `AssertChecksumProvider` 失败 ⇒ **静默回落重路径**
// （实测代价：单文件 meta 常驻 78 MB / 解密解析 ≈584 ms）⇒ 收益归零。

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// fakeChecksumFS 是最小实现：只提供 ChecksumAndSize（其余方法由嵌入接口补）。
type fakeChecksumFS struct {
	FS
	gotRel string
}

func (f *fakeChecksumFS) ChecksumAndSize(_ context.Context, rel string) (string, int64, error) {
	f.gotRel = rel
	return "chk:" + rel, 42, nil
}

// TestPrefixFS_ForwardsChecksumProvider 装饰器必须让可选能力**穿透**。
// 变异：删掉 PrefixFS.ChecksumAndSize ⇒ 本用例红。
func TestPrefixFS_ForwardsChecksumProvider(t *testing.T) {
	t.Parallel()
	inner := &fakeChecksumFS{}
	p := &PrefixFS{FS: inner}
	cp, ok := meta.AssertChecksumProvider(p)
	if !ok {
		t.Fatal("PrefixFS 必须转发 meta.ChecksumProvider（否则调用方静默回落重路径）")
	}
	ck, sz, err := cp.ChecksumAndSize(context.Background(), "a/b.bin")
	if err != nil {
		t.Fatalf("ChecksumAndSize: %v", err)
	}
	if sz != 42 || ck == "" {
		t.Fatalf("应转发到底层，得到 ck=%q sz=%d", ck, sz)
	}
	// 且必须做**路径前缀映射**（与 FileMeta 同口径）。
	if inner.gotRel != p.Key("a/b.bin") {
		t.Fatalf("应转发映射后的路径 %q，实际 %q", p.Key("a/b.bin"), inner.gotRel)
	}
}

// TestPrefixFS_ChecksumProviderUnsupported 底层未实现 ⇒ 明确 ErrUnsupported（不静默成功）。
func TestPrefixFS_ChecksumProviderUnsupported(t *testing.T) {
	t.Parallel()
	p := &PrefixFS{FS: &fakeChecksumFS{}} // 有实现
	_ = p
	plain := &PrefixFS{FS: nil}
	if _, ok := meta.AssertChecksumProvider(plain); !ok {
		t.Fatal("类型断言应成立（方法存在），具体失败由调用返回错误表达")
	}
	if _, _, err := plain.ChecksumAndSize(context.Background(), "x"); err == nil {
		t.Fatal("底层未实现必须返回错误")
	}
}
