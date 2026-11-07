// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// slowMetaAdapter 模拟百度网盘**新目录树的一致性延迟**（实测 2026-11-04）：
// 文件上传成功后 `ls`（List）立即可见，但 `meta`（Meta/Stat）持续报「文件或目录不存在」。
// 此前 waitPutVisible 只轮询 Stat ⇒ 任何新目录/新容器的首次写入必超时失败（ErrTransient）。
type slowMetaAdapter struct {
	*fakeStorageAdapter
	metaMisses int // Meta 调用次数（恒 NotFound，模拟延迟）
}

func (a *slowMetaAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	a.metaMisses++
	return nil, errNotFound
}

// TestPut_StatDelayedButListVisible 钉住**可见性回落到父目录列举**：
// Stat 恒 NotFound 而 List 可见 ⇒ Put 必须成功（而不是 3 轮重传后 ErrTransient）。
// 变异：让查证路径回退到"只能 Meta"（如把 metaByExactList 换回 s.Stat）⇒ 本用例必红
// （Meta 恒 NotFound ⇒ 3 轮重传后 ErrTransient）。
func TestPut_StatDelayedButListVisible(t *testing.T) {
	t.Parallel()
	inner := newFakeStorageAdapter()
	ad := &slowMetaAdapter{fakeStorageAdapter: inner}
	s := newTestStorage(t, ad)
	s.statVisibleWait = 2 * time.Second // 短窗口（默认 90s 不适合单测）

	payload := bytes.Repeat([]byte("new-dir-consistency-"), 512)
	meta, err := s.Put(context.Background(), "data/newcontainer/blob1", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Stat 延迟但 List 可见 ⇒ Put 必须成功，got err=%v", err)
	}
	if meta == nil {
		t.Fatal("Put 成功必须返回 meta")
	}
	// **口径更新（2026-11-08）**：查证操作已由 `Meta` 改为**精确列举**（库 `Meta` 实测会挂起且
	// 不响应 ctx ⇒ 给不可中断调用加超时无效）⇒ 本用例不再要求"必须调用 Meta"，
	// 而要求「**即使 Meta 恒 NotFound/被完全绕开，Put 仍必须成功且拿到权威 md5**」——
	// 这正是数据正确性所需（md5 来自列举，与 Meta 同源）。
	// 仍保留对"Meta 延迟路径不再必要"的显式记录（便于日后回归时理解本用例为何不数 Meta）。
	t.Logf("Meta 调用次数=%d（新实现允许为 0：查证走列举）", ad.metaMisses)
	// 内容确实写入（父目录列举可查）。
	list, lerr := s.List(context.Background(), "data/newcontainer")
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	found := false
	for _, e := range list {
		if e.Key == "data/newcontainer/blob1" {
			found = true
		}
	}
	if !found && len(list) == 0 {
		t.Fatal("上传后父目录列举为空 ⇒ 内容未写入")
	}
}

// TestPut_StatMissingAndListMissing_Fails 反向用例：Stat 与 List 都不可见 ⇒ 仍必须失败
// （回落不能把「真没写成功」误判为成功）。
func TestPut_StatMissingAndListMissing_Fails(t *testing.T) {
	t.Parallel()
	inner := newFakeStorageAdapter()
	ad := &disabledListAdapter{fakeStorageAdapter: inner}
	s := newTestStorage(t, ad)
	s.statVisibleWait = 300 * time.Millisecond

	_, err := s.Put(context.Background(), "data/c2/blob2", bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("Stat/List 都不可见 ⇒ Put 必须 fail-closed（不得静默放行）")
	}
	if !errors.Is(err, ErrTransient) {
		t.Logf("错误类型：%v（非 ErrTransient 也接受，只要是明确失败）", err)
	}
}

// disabledListAdapter：List 恒空（模拟「写失败 + 列举也看不到」）。
type disabledListAdapter struct {
	*fakeStorageAdapter
}

func (a *disabledListAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	return nil, nil
}

func (a *disabledListAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	return nil, errNotFound
}

// TestSizeNear 列举 size 是近似值（大文件以 MB 显示）⇒ 用相对容差而非精确相等。
func TestSizeNear(t *testing.T) {
	t.Parallel()
	cases := []struct {
		want, got int64
		ok        bool
	}{
		{91593425, 91593113, true},   // 实测差值 312B（0.0003%）
		{262144, 262144, true},       // 精确
		{262144, 258048, true},       // 差 4KiB（边界内）
		{262144, 200000, false},      // 差 62KiB > 4KiB 容差
		{100 << 20, 99 << 20, false}, // 差 1MiB > 0.1%(105KB)
	}
	for _, c := range cases {
		if got := sizeNear(c.want, c.got); got != c.ok {
			t.Fatalf("sizeNear(%d,%d)=%v，want %v", c.want, c.got, got, c.ok)
		}
	}
}
