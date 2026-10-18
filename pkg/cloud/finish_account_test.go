// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// finish_account_test.go 钉住「Finish(true) 必须同时回拨 account 与 bucket」（独立复审 P1，2026-10-18）。
//
// 缺陷：`quotaSinkAdapter.Finish(true, oldSize)` 只 `scope.ReleaseUsage(oldSize)` ⇒ 只回拨 bucket，
// **不减 acc.committed** ⇒ 账户与 Scope 分叉（账户恒大于磁盘足迹）。后果：重下前的「唯一权威」
// 回拨（ReleaseAllCommitted）按账户金额释放 ⇒ **多释放** ⇒ 吃掉同桶邻居份额（邻居欠计 ⇒ 短时可越
// owner/卷配额，fail-open）。

import (
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

func TestQuotaSinkAdapter_FinishSuccessReleasesAccountToo(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(10_000)
	sc := pool.EnsureScope([]string{"o"}, 10_000)
	acc := quota.NewTaskAccountReconcile(sc)
	acc.AdjustCommitted(500) // 模拟「已按写入流累计 commit 500」

	ad := &quotaSinkAdapter{acc: acc, scope: sc, w: io.Discard}
	ad.Finish(true, 300) // 完成：丢弃了 300 字节既有产物

	// 账户与 bucket 必须同源（两者都减 300）。
	if got := acc.Committed(); got != 200 {
		t.Fatalf("Finish(true,300) 后账户 committed=%d, want 200"+
			"（未修必红：只回拨 bucket ⇒ 账户虚高 ⇒ 后续权威回拨多吃同桶邻居份额）", got)
	}
}
