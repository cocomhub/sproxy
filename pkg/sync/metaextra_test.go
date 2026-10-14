// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"testing"
)

// recordingMetaFS 记录旁路 Extra 更新（FS 方法用嵌入接口占位，测试不调用）。
type recordingMetaFS struct {
	FS
	gotRel  string
	gotKeys []string
}

func (f *recordingMetaFS) UpdateMetaExtra(_ context.Context, rel string, extra map[string]any) error {
	f.gotRel = rel
	for k := range extra {
		f.gotKeys = append(f.gotKeys, k)
	}
	return nil
}

// bareFS 仅实现基础 FS（不实现任何可选能力）。
type bareFS struct{ FS }

// TestPrefixFS_UpdateMetaExtra_ForwardsInBucketKeySpace 旁路 Extra 更新必须（1）转发到
// 内层、（2）rel 映射到桶键空间（否则 damaged 标记会写到用户可见路径/错误键）。
// 变异：去掉 p.Key(rel) 直接用 rel → 断言红。
func TestPrefixFS_UpdateMetaExtra_ForwardsInBucketKeySpace(t *testing.T) {
	t.Parallel()
	rec := &recordingMetaFS{}
	p := NewPrefixFS(rec, "alice/user/")
	if err := p.UpdateMetaExtra(context.Background(), "docs/a.bin", map[string]any{"source_integrity": "damaged"}); err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	if rec.gotRel != "alice/user/docs/a.bin" {
		t.Fatalf("rel 应映射为桶键 alice/user/docs/a.bin, got %q", rec.gotRel)
	}
	if len(rec.gotKeys) != 1 || rec.gotKeys[0] != "source_integrity" {
		t.Fatalf("extra 应原样转发, got %v", rec.gotKeys)
	}
}

// TestPrefixFS_UpdateMetaExtra_UnsupportedIsDistinct 底层无该能力 → ErrUnsupported
// （能力探测语义：未实现必须与「空操作成功」可区分，否则上游以为标记已写入）。
func TestPrefixFS_UpdateMetaExtra_UnsupportedIsDistinct(t *testing.T) {
	t.Parallel()
	p := NewPrefixFS(&bareFS{}, "alice/user/")
	err := p.UpdateMetaExtra(context.Background(), "docs/a.bin", map[string]any{"k": "v"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("底层未实现应返回 ErrUnsupported, got %v", err)
	}
}

// TestAssertMetaExtraUpdater_Unwraps 能力探测须穿过装饰器链（否则上游恒判定「无能力」→
// 静默丢失标记）。
func TestAssertMetaExtraUpdater_Unwraps(t *testing.T) {
	t.Parallel()
	rec := &recordingMetaFS{}
	p := NewPrefixFS(NewPrefixFS(rec, "a/"), "b/")
	u, ok := AssertMetaExtraUpdater(p)
	if !ok || u == nil {
		t.Fatalf("应穿过装饰器链找到旁路 Extra 能力: ok=%v", ok)
	}
	if _, ok := AssertMetaExtraUpdater(&bareFS{}); ok {
		t.Fatal("未实现时应返回 ok=false")
	}
	if _, ok := AssertMetaExtraUpdater(nil); ok {
		t.Fatal("nil FS 应返回 ok=false")
	}
}
