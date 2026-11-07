// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"testing"
)

// rename_rmdir_charge_test.go —— 钉住**rename/rmdir 释放（与配额转移）的口径**。
//
// 背景（真实缺陷，2026-10-17 审核发现，与 #822/#823 同类）：同一业务链上存在**两套口径**——
// 跨卷 move 走 `physChargeFor`（**卷自报足迹优先**），而 home 卷 rename（`renameWithQuotaTransfer`）
// 与 rmdir 清理（`cleanupRemovedDir`/`removeDirOnVolume`）直接用 `Stat`/`ReadDir` 尺寸。
// 对 **secretdata** 这类封装卷，`Stat` 只含**内容容器**（不含 XOR parity、不含 pad 到
// [base,2·base) 的 meta blob）⇒ 结算按足迹、释放按 Stat ⇒ 每次操作泄漏 `(足迹 − Stat)` 的
// **幽灵占用**（假 507）。
//
// 夹具注意（#822/#823 教训）：释放发生在对象**已消失之后**（rename 已改名 / rmdir 已 RemoveAll），
// 存在性感知的替身会判「无足迹」而回落 ⇒ **无判别力**。故写入阶段用 `fakePhysFootprint`
// （存在性感知，保证 prev=0），进入释放阶段前换成 `constPhysFootprint`（恒报足迹）。
//
// 变异：把 `releaseChargeFor` 改回直接用 `Stat`/`f.size` ⇒ 两条均红。

// TestRemoveDir_VolumeReportedFootprintWins：rmdir 的 per-file 释放必须按卷自报足迹。
func TestRemoveDir_VolumeReportedFootprintWins(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	const foot = int64(3 << 20)
	e.svc.rt.phys = fakePhysFootprint{foot: foot, root: e.tenantFor("alice").Root()}

	payload := bytes.Repeat([]byte("d"), 512)
	e.upload(t, "alice", "sub/f.bin", payload, sha256Hex(payload), 0)
	sc := e.quotaScopeFor("alice", "user/sub/f.bin")
	if sc == nil {
		t.Fatal("Scope 不可用（测试前提不成立）")
	}
	if got := sc.Usage(); got != foot {
		t.Fatalf("写入后 Scope=%d want %d", got, foot)
	}

	// 释放阶段：子树已被 RemoveAll ⇒ 换恒报足迹的替身（同上理由）。
	e.svc.rt.phys = constPhysFootprint{foot: foot}
	if _, err := e.svc.RemoveDir(context.Background(), "alice", "sub", "", true); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}
	if got := sc.Usage(); got != 0 {
		t.Fatalf("rmdir 后 Scope=%d want 0（per-file 释放必须按卷自报足迹，而非 ReadDir 内容容器尺寸 %d）",
			got, len(payload))
	}
}
