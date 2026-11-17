// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// humanSizeListAdapter 复现生产形态（2026-11-17）：上传成功，但父目录列举（binary `ls -l`）
// 只给**人类可读**的 2 位小数大小（如 `1.37MB`）⇒ 解析出的 `Size` 带**半个刻度的舍入误差**
// （`SizeSlack`）。Meta 恒 NotFound（新目录树一致性延迟）⇒ 复核走列举回落路径。
type humanSizeListAdapter struct {
	*fakeStorageAdapter
	remoteSize  int64
	remoteSlack int64
	base        string // 目标 blob 名（须与被查 key 的 base 一致，否则列举不算「可见」）
}

func (a *humanSizeListAdapter) Meta(context.Context, string) (*ObjectMeta, error) {
	return nil, errNotFound // 新目录树：meta 报 31066（实测 5–10 分钟才可见）
}

func (a *humanSizeListAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 模拟 `ls -l` 的显示值：真实 size 经「2 位小数」四舍五入后再解析回来。
	return []ObjectMeta{{
		Key:       remotePath + "/" + a.base,
		Size:      a.remoteSize,
		SizeSlack: a.remoteSlack,
	}}, nil
}

// TestPut_HumanRoundedListSizeIsNotMismatch 钉住生产根因与修复（2026-11-17 实测）：
//
//	`want_size=1432234, got_size=1436549`（= `int64(1.37 * 1<<20)`，即显示值四舍五入的结果）
//	差值 4315B > 复核容差 4096B ⇒ 有界重传 3 轮 ⇒ `ErrTransient` ⇒ **写入 HTTP 500** ✗
//
// 根因不是内容不一致，而是**把显示四舍五入值当字节精确值比较**（错误比较 ✗）。
// 修复：`ObjectMeta.SizeSlack`（显示误差上界）+ `sizeMatches`/`sizeNear` 计入该界
// ⇒ 列举回落路径应**判一致并通过**。
//
// 变异：`sizeNear` 不计 slack（或 `parseHumanSize` 返回 slack=0）⇒ 本用例红（3 轮重传后 ErrTransient，
// 与生产症状一字不差）。
func TestPut_HumanRoundedListSizeIsNotMismatch(t *testing.T) {
	t.Parallel()
	const wantSize = 1432234 // 本地精确字节数
	// 生产实测的远端值：显示串 "1.37MB" 解析而来（携带同样的显示误差上界）。
	humanRemote, humanSlack := parseHumanSize("1.37MB")
	if humanRemote != 1436549 {
		t.Fatalf("夹具前提错：parseHumanSize(\"1.37MB\") = %d, want 1436549（生产实测值）", humanRemote)
	}
	if humanSlack != 5242 {
		t.Fatalf("夹具前提错：显示误差上界 = %d, want 5242（0.005 × 1 MiB）", humanSlack)
	}
	if humanRemote <= wantSize || int64(humanRemote-wantSize) <= 4096 {
		t.Fatalf("夹具未复现根因：差值 %d 必须 > 4096B 容差", humanRemote-wantSize)
	}

	inner := newFakeStorageAdapter()
	ad := &humanSizeListAdapter{
		fakeStorageAdapter: inner,
		remoteSize:         humanRemote,
		remoteSlack:        humanSlack,
		base:               "human-blob",
	}
	s := newTestStorage(t, ad)
	s.statVisibleWait = 200 * time.Millisecond

	payload := bytes.Repeat([]byte("h"), wantSize)
	if _, err := s.Put(context.Background(), "data/dir/human-blob", bytes.NewReader(payload)); err != nil {
		t.Fatalf("显示舍入值（带 %dB 误差界）必须判一致 ⇒ Put 应成功，got err=%v", humanSlack, err)
	}
}
