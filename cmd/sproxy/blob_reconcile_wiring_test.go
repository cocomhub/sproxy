// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs"
)

// fakeReconcileCap 是 ReconcileCapable 的测试替身（记录调用、可控 pending 集合）。
type fakeReconcileCap struct {
	pending  []baidupcs.PendingReconcile
	retryErr error
	gotRel   string
	gotCopy  string
	removed  []string
}

func (f *fakeReconcileCap) RetryBlobMD5Reconcile(_ context.Context, key, localCopy string) error {
	f.gotRel, f.gotCopy = key, localCopy
	return f.retryErr
}
func (f *fakeReconcileCap) PendingReconcile() []baidupcs.PendingReconcile { return f.pending }
func (f *fakeReconcileCap) RemovePendingReconcile(localCopy string) error {
	f.removed = append(f.removed, localCopy)
	return nil
}

// TestBlobReconcileWiring_按rel反查副本并路由 锁定接线器的核心契约：
//   - 按**卷名**路由到对应能力（卷未登记 ⇒ 报错转人工，绝不静默成功）；
//   - 副本**按 (volume, rel) 反查**（不是由事项携带）⇒ 事项 Path 可用稳定的 rel；
//   - 反查不到副本 ⇒ 报错（无源可传 ⇒ 转人工）。
func TestBlobReconcileWiring_按rel反查副本并路由(t *testing.T) {
	t.Parallel()
	capA := &fakeReconcileCap{pending: []baidupcs.PendingReconcile{{Key: "jav/a.bin", LocalCopy: "/tmp/p1/blob"}}}
	w := newBlobReconcileWiring(nil)
	w.register("volA", capA)

	if err := w.retry(context.Background(), "volA", "jav/a.bin"); err != nil {
		t.Fatalf("应路由成功，得到 %v", err)
	}
	if capA.gotRel != "jav/a.bin" || capA.gotCopy != "/tmp/p1/blob" {
		t.Fatalf("反查/透传错位：rel=%q copy=%q", capA.gotRel, capA.gotCopy)
	}

	// 卷未登记 ⇒ 错误（转人工）
	if err := w.retry(context.Background(), "volX", "jav/a.bin"); err == nil {
		t.Fatal("未登记卷必须报错")
	}
	// rel 无对应副本 ⇒ 错误（无源可传）
	if err := w.retry(context.Background(), "volA", "jav/missing.bin"); err == nil {
		t.Fatal("副本不存在必须报错")
	}
	// 能力返回错误 ⇒ 原样上抛（供 issues 退避）
	capA.retryErr = errors.New("仍未收敛")
	if err := w.retry(context.Background(), "volA", "jav/a.bin"); !errors.Is(err, capA.retryErr) {
		t.Fatalf("动作错误应透传，得到 %v", err)
	}
}

// TestBlobReconcileWiring_启动扫描重登记 锁定⑥：启动扫描把各卷 pending 项重新登记为事项，
// 并把**无法定位（rel/副本为空）**的条目标记为 skipped（不猜、不静默删）。
func TestBlobReconcileWiring_启动扫描重登记(t *testing.T) {
	t.Parallel()
	var reported [][2]string
	w := newBlobReconcileWiring(func(volume, rel, localCopy string) {
		reported = append(reported, [2]string{volume, rel})
	})
	w.register("volA", &fakeReconcileCap{pending: []baidupcs.PendingReconcile{
		{Key: "jav/a.bin", LocalCopy: "/tmp/p1/a"},
		{Key: "", LocalCopy: "/tmp/p1/broken"}, // 元数据损坏 ⇒ 跳过
	}})
	w.register("volB", &fakeReconcileCap{pending: []baidupcs.PendingReconcile{
		{Key: "jav/c.bin", LocalCopy: "/tmp/p2/c"},
	}})

	reportedN, skipped := w.scanAndReport()
	if reportedN != 2 || skipped != 1 {
		t.Fatalf("扫描计数错误：reported=%d skipped=%d（期望 2/1）", reportedN, skipped)
	}
	if len(reported) != 2 {
		t.Fatalf("应上报 2 条，实际 %d", len(reported))
	}
	// 卷名必须如实带上（重试路由依赖它）
	joined := reported[0][0] + "|" + reported[1][0]
	if !strings.Contains(joined, "volA") || !strings.Contains(joined, "volB") {
		t.Fatalf("上报卷名丢失：%v", reported)
	}
}

// TestBlobReconcileWiring_未登记对象被忽略 锁定：不支持该能力的后端（非 baidupcs）注册时被静默忽略，
// 不会凭空获得"重试能力"（仅 baidu 后端启用）。
func TestBlobReconcileWiring_未登记对象被忽略(t *testing.T) {
	t.Parallel()
	w := newBlobReconcileWiring(nil)
	w.register("volX", struct{}{}) // 不实现 ReconcileCapable
	if err := w.retry(context.Background(), "volX", "r"); err == nil {
		t.Fatal("非能力对象注册后不得获得重试路由")
	}
}
