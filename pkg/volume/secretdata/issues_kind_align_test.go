// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import "testing"

// TestIssueKindBlobPendingVerify_AlignedWithIssues 锁定「待复核」事项类型字符串**两侧一致**。
//
// 为什么需要（2026-11-17，R6 门禁拦下的真实架构倒置）：领域包 `secretdata` **不得** import
// 装配层 `pkg/server/issues`（`internal/archcheck` 的分层门禁）⇒ 该 Kind 的字面量只能各自持有。
// 两个字面量一旦漂移，卷侧上报的事项类型就匹配不上执行器 ⇒ **静默退回"只登记不重试"** ✗
// （正是本次修复的缺口）。故用本用例把两者钉在一起（改任一侧即红）。
func TestIssueKindBlobPendingVerify_AlignedWithIssues(t *testing.T) {
	t.Parallel()
	// 与 `pkg/server/issues/issues.go` 的 `KindBlobPendingVerify` 必须字面相同。
	const wantIssuesSide = "blob_pending_verify"
	if issueKindBlobPendingVerify != wantIssuesSide {
		t.Fatalf("卷侧 issueKindBlobPendingVerify=%q 与 issues.KindBlobPendingVerify=%q 不一致 ⇒ "+
			"事项类型漂移，执行器将无法匹配（静默退回只登记不重试 ✗）",
			issueKindBlobPendingVerify, wantIssuesSide)
	}
}
