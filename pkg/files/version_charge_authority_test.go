// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import "testing"

// version_charge_authority_test.go —— 钉住**版本字节的记账口径**。
//
// 背景（真实缺陷，2026-10-17 审核发现）：`SaveVersion` 取源文件 `srcSize` 用
// `tnt.Root().Stat()`，提交/释放用 `physicalCharge(root, written)`（**公式**折算）。对
// **secretdata** 卷这**两处都覆盖不到真实物理占用**——其 `Stat` 只含内容容器（**不含** meta
// blob pad，也不含 XOR parity），公式更是只按长度线性折算。而 versioning 由**全局配置**
// `Versioning.Enabled` 控制（**非按卷**）⇒ secretdata 卷**可达** ⇒ 版本字节**少计** ⇒
// owner 配额与卷容量池低于真实占用 ⇒ **可超限**（不安全方向；与 `prevCharge` 注释所述
// 「少释放 = 幽灵占用/假 507」同源）。
//
// 口径与写路径一致（`settleSize`）：**卷自报实测足迹优先**，公式只作能力缺失时的回落。
//
// 变异：把提交侧改回 `physicalCharge(root, written)`（去掉足迹优先）⇒ 红。

// constPhysFootprint 是**不做存在性判定**的足迹替身：任何路径恒报告 foot。
//
// 为什么需要它：`fakePhysFootprint` 的存在性判定硬编码了 `user/` 前缀（只适用于 user 桶路径），
// 而版本文件的键在 **version 桶**（`version/<userRel>/<id>`）⇒ 会被误判为「不存在」而回落公式。
// 覆盖写阶段目标文件确实已存在（prev 取 foot 正确），故此处无需存在性判定。
type constPhysFootprint struct{ foot int64 }

func (f constPhysFootprint) PhysFootprint(_, _, _ string) (int64, bool) { return f.foot, true }

func (f constPhysFootprint) PhysBound(_ string, logical int64) int64 { return logical + f.foot }

func (f constPhysFootprint) DirChainFootprint(_, _, _ string) (int64, bool) { return 0, false }

func (f constPhysFootprint) DirPhysBound(_ string) int64 { return 0 }

// TestSaveVersion_VolumeReportedFootprintWins：覆盖写触发保存版本后，version 桶的记账必须等于
// **卷自报足迹**，而非逻辑长度/公式折算值。
func TestSaveVersion_VolumeReportedFootprintWins(t *testing.T) {
	t.Parallel()
	e := writeTargetEnv(t)
	e.versioningEnabled = true
	// 足迹替身：固定报告 3MiB（模拟 secretdata 上小文件因 meta pad + parity 的物理膨胀）。
	// root 非 nil ⇒ 按对象存在性报告（与生产语义同构）。
	const foot = int64(3 << 20)
	e.svc.rt.phys = fakePhysFootprint{foot: foot, root: e.tenantFor("alice").Root()}

	b1 := []byte("version-one")
	b2 := []byte("version-two")
	e.upload(t, "alice", "a.txt", b1, sha256Hex(b1), 0)
	// 覆盖写阶段：目标文件已存在 ⇒ 换成无存在性判定的替身（版本桶键不在 user/ 前缀下）。
	e.svc.rt.phys = constPhysFootprint{foot: foot}
	e.upload(t, "alice", "a.txt", b2, sha256Hex(b2), 0) // 覆盖 ⇒ SaveVersion 保存旧版本

	vsc := e.quotaScopeFor("alice", "version")
	if vsc == nil {
		t.Fatal("version 桶 Scope 不可用（测试前提不成立）")
	}
	if got := vsc.Usage(); got != foot {
		t.Fatalf("version 桶 Usage=%d want %d（版本是源文件的同内容拷贝 ⇒ 必须按卷自报足迹记账，"+
			"而非逻辑长度 %d/公式折算值）", got, foot, len(b1))
	}
}
