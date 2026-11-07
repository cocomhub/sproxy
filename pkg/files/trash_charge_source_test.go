// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// trash_charge_source_test.go 钉住「清空回收站时的**释放金额与写入/结算同源**」
// （独立复审「释放≠结算」类根治，2026-10-18）。
//
// 缺陷：releaseTrashEntryQuota 只用**公式折算**（physicalCharge）⇒ 对 `footprint ≠ 公式` 的卷
// （secretdata 的 parity + meta pad）**少释放** ⇒ 幽灵占用/假 507。软删条目此刻**仍存在于 trash
// 路径**（下面才 Remove）⇒ 可对其自身路径取卷自报实测足迹（PhysFootprint 按 rel 查索引，改名后
// 索引随迁）。修法：优先 `physChargeFor`（同源），能力缺失/不在索引 ⇒ 回落原公式。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestEmptyTrash_ReleaseUsesVolumeFootprintNotFormula(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	const footprint = int64(5000)
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant 不可用")
	}
	env.phys = trashFootprintStub{foot: footprint}
	env.rebuild()
	userAbs, _ := tnt.Root().Abs("user")
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	const content = "hello"
	if err := os.WriteFile(filepath.Join(userAbs, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 模拟写入侧结算：按**卷自报足迹** commit（与生产 settleSize 同源）。
	scope := env.svc.rt.quotaScope("alice", "user/a.txt")
	if scope == nil {
		t.Fatal("前置：user 桶配额 Scope 不可用")
	}
	scope.Adjust(0, footprint)
	if got := scope.Usage(); got != footprint {
		t.Fatalf("前置 Usage=%d want %d", got, footprint)
	}

	// 软删 → 清空回收站。
	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "a.txt",
		ExpectedChecksum: testutil.SHA256Hex([]byte(content)), SoftDelete: true,
	}); err != nil {
		t.Fatalf("DeleteFile soft: %v", err)
	}
	if err := env.svc.EmptyTrash(context.Background(), "alice"); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}

	// **核心断言**：释放金额必须等于写入侧记账的**实测足迹** ⇒ 用量归零。
	// 未修必红：公式只释放逻辑长度 5 ⇒ 残留 4995 幽灵占用（假 507 的来源）。
	if got := scope.Usage(); got != 0 {
		t.Fatalf("清空回收站后 Usage=%d want 0"+
			"（未修必红：按公式释放 ⇒ 对 footprint≠公式 的卷少释放 ⇒ 幽灵占用/假 507）", got)
	}
}

// trashFootprintStub 报告固定足迹，**不做路径前缀假设**——与生产 `PhysFootprint` 同构：它按 rel
// 直接查索引（软删改名后的 trash 路径同样可查）。既有 `fakePhysFootprint` 硬编码 "user/" 前缀，
// 对 trash 路径必然落空 ⇒ 用它会让本用例**永远走回落分支**（夹具不同构 ⇒ 假绿）。
type trashFootprintStub struct{ foot int64 }

func (f trashFootprintStub) PhysFootprint(_, _, rel string) (int64, bool) {
	if f.foot <= 0 || rel == "" {
		return 0, false
	}
	return f.foot, true
}

func (f trashFootprintStub) PhysBound(_ string, logical int64) int64 { return logical }

func (f trashFootprintStub) DirChainFootprint(_, _, _ string) (int64, bool) { return 0, false }

func (f trashFootprintStub) DirPhysBound(_ string) int64 { return 0 }
