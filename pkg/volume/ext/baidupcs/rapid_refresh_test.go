// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// rapidRecorder 记录 RapidUpload 调用（实现 rapidUploader 可选能力）。
type rapidRecorder struct {
	mu    sync.Mutex
	calls []*stagedUpload
	hit   bool
}

func (r *rapidRecorder) RapidUpload(_ context.Context, _ string, st *stagedUpload) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *st
	r.calls = append(r.calls, &cp)
	return r.hit, nil
}

func (r *rapidRecorder) snapshot() []*stagedUpload {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*stagedUpload, len(r.calls))
	copy(out, r.calls)
	return out
}

// rapidAdapter 在 fake adapter 之上实现可选能力 rapidUploader（记录调用 + 可控命中）。
type rapidAdapter struct {
	*fakeStorageAdapter
	rec *rapidRecorder
}

func (a *rapidAdapter) RapidUpload(ctx context.Context, remote string, st *stagedUpload) (bool, error) {
	return a.rec.RapidUpload(ctx, remote, st)
}

// TestStorage_RapidRefresh 钉住**秒传刷新优先**的后端契约（2026-11-04 实测方法：
// 百度分片上传后 ETag 可能不可靠，内容却在盘 ⇒ 用 rapidupload 零传输刷新，禁止退化成重传）。
//
// 覆盖：
//  1. 小块（≤256KiB）⇒ 用 md5 同时作 sliceMD5/blockMD5s 调用 RapidUpload，命中返回 true；
//  2. 大块（>256KiB）⇒ **不调用**（缺 sliceMD5/blockMD5s），返回 false 让调用方回落重传；
//  3. 缺 md5/size ⇒ 不调用。
func TestStorage_RapidRefresh(t *testing.T) {
	t.Parallel()
	rec := &rapidRecorder{hit: true}
	// adapter 需实现可选能力 rapidUploader（rapidUploaderOf 从 adapter 探测）。
	base := newFakeStorageAdapter()
	s := newTestStorage(t, &rapidAdapter{fakeStorageAdapter: base, rec: rec})

	// 1) 小块：应命中并带上正确参数
	hit, err := s.RapidRefresh(context.Background(), syncpkg.VerifyItem{
		Key: "data/c1/blob1", MD5: "0123456789abcdef0123456789abcdef", Size: 4096, CRC32: "123456",
	})
	if err != nil {
		t.Fatalf("RapidRefresh: %v", err)
	}
	if !hit {
		t.Fatal("小块应命中秒传刷新")
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应调用 1 次 RapidUpload，got %d", len(calls))
	}
	st := calls[0]
	if st.md5 != "0123456789abcdef0123456789abcdef" || st.sliceMD5 != st.md5 || len(st.blockMD5s) != 1 || st.crc32 != "123456" || st.size != 4096 {
		t.Fatalf("刷新参数不正确：md5=%s slice=%s blocks=%v crc=%s size=%d", st.md5, st.sliceMD5, st.blockMD5s, st.crc32, st.size)
	}

	// 2) 大块（> 256KiB）：不调用（缺 sliceMD5/blockMD5s），交由调用方重传
	before := len(rec.snapshot())
	hit, err = s.RapidRefresh(context.Background(), syncpkg.VerifyItem{
		Key: "data/c1/blob2", MD5: "fedcba9876543210fedcba9876543210", Size: rapidRefreshMaxSize + 1,
	})
	if err != nil || hit {
		t.Fatalf("大块不应刷新：hit=%v err=%v", hit, err)
	}
	if after := len(rec.snapshot()); after != before {
		t.Fatalf("大块不应调用 RapidUpload（%d → %d）", before, after)
	}

	// 3) 缺参数：不调用
	if hit, _ := s.RapidRefresh(context.Background(), syncpkg.VerifyItem{Key: "k", Size: 0}); hit {
		t.Fatal("缺 md5/size 不应刷新")
	}
}

// healAdapter 模拟「刷新前 md5 不可靠、刷新后变权威」的远端：
// Meta 在首次刷新前返回错误 ETag；RapidUpload 命中后切换为权威 ETag。
type healAdapter struct {
	*fakeStorageAdapter
	mu            sync.Mutex
	refreshed     bool
	authoritative string
}

func (a *healAdapter) RapidUpload(_ context.Context, _ string, st *stagedUpload) (bool, error) {
	a.mu.Lock()
	a.authoritative = st.md5
	a.refreshed = true
	a.mu.Unlock()
	return true, nil
}

func (a *healAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refreshed {
		return &ObjectMeta{Key: remotePath, Size: 4096, ETag: a.authoritative}, nil
	}
	// 刷新前：内容在盘但 md5 不可靠
	return &ObjectMeta{Key: remotePath, Size: 4096, ETag: "00000000000000000000000000000000"}, nil
}

// TestVerifyMany_SelfHealsByRapidRefresh 钉住**后端自愈内置**：
// 校验发现 ETag 不一致时，`VerifyMany` 必须**自己**先做秒传刷新（零传输）并复验，
// 返回 State=OK + Refreshed=true——使用方（secretdata/上层）**不需要**编排刷新逻辑。
//
// 变异：去掉 verifyOne 里的 healByRapidRefresh 调用 ⇒ 返回 mismatch ⇒ 本用例必红。
func TestVerifyMany_SelfHealsByRapidRefresh(t *testing.T) {
	t.Parallel()
	base := newFakeStorageAdapter()
	base.files["data/c1/blob1"] = "0123456789abcdef0123456789abcdef"
	ad := &healAdapter{fakeStorageAdapter: base}
	s := newTestStorage(t, ad)

	items := []syncpkg.VerifyItem{{
		Key: "data/c1/blob1", MD5: "0123456789abcdef0123456789abcdef", Size: 4096, CRC32: "999",
	}}
	results := s.VerifyMany(context.Background(), items)
	if len(results) != 1 {
		t.Fatalf("应返回 1 条结果，got %d", len(results))
	}
	r := results[0]
	if r.State != syncpkg.VerifyOK {
		t.Fatalf("后端应自愈为 OK，got state=%s reason=%s", r.State, r.Reason)
	}
	if !r.Refreshed {
		t.Fatal("自愈必须标记 Refreshed=true（使用方据此可知走了零传输刷新）")
	}
	if !ad.refreshed {
		t.Fatal("夹具失效：未调用 RapidUpload")
	}
}
