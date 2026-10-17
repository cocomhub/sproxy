// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// delete_charge_authority_test.go —— 钉住删除释放金额的**权威优先级**。
//
// 背景（真实缺陷，2026-10-17 审核发现）：`removeOrSoftDelete` 的释放金额本应是
// `DeleteFileInput.chargedHint`（删除**前**捕获、与结算同源 ⇒ 权威），但紧随 `switch` 之后
// 曾残留一个**无条件** `physChargeFor` 调用，会把它**覆盖**掉。生产卷在 quarantine 改名后按
// 原路径取不到足迹（`ok=false`）⇒ 该覆盖当前多为冗余；但「足迹解析不依赖 stat」的卷型
// （索引/sidecar 型）会让权威值被**静默覆盖**为过期值 ⇒ 释放量错误（少释放 = 幽灵占用/假 507；
// 多释放 = 配额虚低 ⇒ 超限写入通过）。
//
// 本用例用**序列化**假能力把该分歧放大成可判定信号：同一次删除里第 1 次查询（删除前捕获
// chargedHint）返回 100，第 2 次查询（若仍被无条件调用）返回 999。
//   - 修复后：释放 100 ⇒ Usage 归零；
//   - 残留无条件覆盖：释放 999 ⇒ Usage 不为零（红）。
//
// 变异：把无条件 `physChargeFor` 覆盖加回 ⇒ 红。

// pathPhysFootprint 是**按调用次序**返回足迹的假能力（指针接收者 ⇒ 状态在多次调用间保持）：
// 第 1 次（删除**前**捕获 chargedHint）返回 orig，其后（若实现里仍有无条件覆盖，会在删除中
// 间态二次查询）返回 moved。
//
// 为什么用序列而非路径：实测 `f.remotePath` 在 `removeOrSoftDelete` 里仍是**原路径** ⇒ 按路径
// 无法区分「删除前捕获」与「删除后二次查询」（两者同路径、同来源 ⇒ 同值）。序列是唯一能
// 把该分歧放大成可判定信号的构造。
//
// moved 必须**小于** orig：`ReleaseUsage` 超量会被钳到 0（实测），用更大的 moved 会让两种实现
// 都收敛到 0 ⇒ 无判别力。
type pathPhysFootprint struct {
	orig  int64
	moved int64
	calls int
}

func (f *pathPhysFootprint) PhysFootprint(_, _, _ string) (int64, bool) {
	f.calls++
	if f.calls == 1 {
		return f.orig, true
	}
	return f.moved, true
}

// 其余三个方法：本用例只关心**文件**足迹 ⇒ 目录侧恒无能力（与生产「明文卷无目录成本」同构）。
func (f *pathPhysFootprint) PhysBound(_ string, logical int64) int64 { return logical }

func (f *pathPhysFootprint) DirChainFootprint(_, _, _ string) (int64, bool) { return 0, false }

func (f *pathPhysFootprint) DirPhysBound(_ string) int64 { return 0 }

// TestDeleteFile_ChargedHintIsAuthoritative：删除释放金额必须取 `chargedHint`（删除前捕获的
// 权威值），不得被删除后的二次足迹查询覆盖。
func TestDeleteFile_ChargedHintIsAuthoritative(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("q"), 512)

	// 写入阶段：用**存在性感知**的替身（与生产语义同构）⇒ 新文件 prev=0、结算取足迹 100。
	e.svc.rt.phys = fakePhysFootprint{foot: 100, root: e.tenantFor("alice").Root()}
	if _, err := e.svc.WriteTarget(ctx, WriteTargetInput{
		Owner: "alice", RemotePath: "auth.bin", ClientSize: int64(len(payload)),
	}, bytes.NewReader(payload)); err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	sc := e.quotaScopeFor("alice", "user/auth.bin")
	if sc == nil {
		t.Fatal("user 桶 Scope 不可用（测试前提不成立）")
	}
	if got := sc.Usage(); got != 100 {
		t.Fatalf("写入后 Usage=%d want 100", got)
	}

	// 删除阶段：第 1 次查询（删除**前**捕获 chargedHint）= 100；若实现里仍有无条件覆盖，
	// 删除中间态的二次查询会取到 40 并把释放金额改写掉。
	// moved=40 < orig=100：避免 `ReleaseUsage` 超量钳位掩盖差异。
	e.svc.rt.phys = &pathPhysFootprint{orig: 100, moved: 40}
	if _, err := e.svc.DeleteFile(ctx, DeleteFileInput{
		Owner: "alice", RemotePath: "auth.bin", SoftDelete: false,
		ExpectedChecksum: testutil.SHA256Hex(payload),
	}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if got := sc.Usage(); got != 0 {
		t.Fatalf("删除后 Usage=%d want 0（释放金额必须取 chargedHint=100，不得被删除中间态的二次查询覆盖为 40）", got)
	}
}
