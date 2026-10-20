// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// move_quota_phys_test.go —— 跨卷 move 的记账必须按**物理**口径（两侧各自折算）。
//
// 缺陷（2026-10-16 高风险区审核发现，第三个「单位不一致」）：
// 源/目标两侧此前都按 `written`（= `userVisibleSize` ⇒ **用户可见长度/明文**）入账，而
// **目标**经 `writeUserVisible` **透明加密**落盘（物理 > 逻辑）⇒ **两侧都漂移**：
//   - to 侧（charge）：少记 `(物理 − 逻辑)` ⇒ owner Scope 少记 ⇒ **可超真实配额**（G1）；
//   - from 侧（release）：源内容在**上传时**已按物理入账（`files.settleSize`）⇒ 按逻辑释放
//     会**少释放** ⇒ **幽灵占用/假 507**（方向与 to 侧相反）。
// 注意 `size`/`written` 必须是**明文口径**（`crossVolumeCopy` 经解密流复制，TOCTOU 等值守卫
// `written != size` 依赖同口径）——折算只能发生在**账本边界**，不得改动复制语义。
//
// 修：`moveFileCtx.toCharge`/`fromCharge`（= `files.RootPhysSizer` 同源折算）+ 预留点
// `moveReserveTarget` 入账前折算。
//
// 变异：`toCharge`/`fromCharge` 改回恒等（或预留去掉折算）⇒ 断言红。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestMoveReserveTarget_ChargedPhysicalBytes 预留（账本边界）必须按目标卷物理口径。
func TestMoveReserveTarget_ChargedPhysicalBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "dst.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "dst", Root: dir, Extra: map[string]any{
		"encrypt":          true,
		"encrypt_key_file": keyFile,
	}}}
	h := buildVolSetHandlers(t, cfg)
	tnt := h.volumeTenant("dst", "alice")
	if tnt == nil || tnt.Root() == nil || !tnt.Root().IsEncrypted() {
		t.Fatal("夹具前提不成立：目标卷应为已装配的加密卷")
	}
	const size = 8192 // 用户可见长度（明文）
	_, _, scope, _, _, ok := h.moveReserveTarget(context.Background(), "alice", "user/f.bin", "dst", tnt.Root(), size)
	if !ok {
		t.Fatal("预留失败（夹具/配额问题）")
	}
	if got, want := scope.Reserved(), storage.EncryptedPhysicalSize(size); got != want {
		t.Fatalf("跨卷 move 预留必须按目标卷**物理**口径: reserved=%d want=%d（逻辑=%d）", got, want, size)
	}
}

// TestMoveCtx_ChargeIsPhysicalBothSides 钉住两侧折算本身（to = 目标卷口径、from = 源卷口径），
// 并钉住**未加密卷恒等**（零回归）。
//
// 覆盖边界：本用例覆盖折算逻辑与预留边界；`moveCommitRelease` / `moveCommitConcurrentDeleted`
// 的 commit/release 调用点复用**同一** `toCharge`/`fromCharge`，其集成行为由既有跨卷 move 用例
// （`volumes_api_test.go` 等）与对账矩阵覆盖。
func TestMoveCtx_ChargeIsPhysicalBothSides(t *testing.T) {
	t.Parallel()
	mk := func(enc bool) *storage.Root {
		t.Helper()
		r, err := storage.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		t.Cleanup(func() { _ = r.Close() })
		if enc {
			if serr := r.SetEncryption(bytes.Repeat([]byte{9}, 32)); serr != nil {
				t.Fatalf("SetEncryption: %v", serr)
			}
		}
		return r
	}
	const n = 8192
	encTo, encFrom := mk(true), mk(true)
	mc := &moveFileCtx{toRoot: encTo, fromRoot: encFrom}
	if got, want := mc.toCharge(n), storage.EncryptedPhysicalSize(n); got != want {
		t.Fatalf("to 侧必须按目标卷物理: got=%d want=%d", got, want)
	}
	if got, want := mc.fromCharge(n), storage.EncryptedPhysicalSize(n); got != want {
		t.Fatalf("from 侧必须按源卷物理: got=%d want=%d", got, want)
	}
	// 未加密卷 ⇒ 恒等（零回归）。
	plain := &moveFileCtx{toRoot: mk(false), fromRoot: mk(false)}
	if got := plain.toCharge(n); got != n {
		t.Fatalf("非加密目标卷必须恒等: got=%d want=%d", got, n)
	}
	if got := plain.fromCharge(n); got != n {
		t.Fatalf("非加密源卷必须恒等: got=%d want=%d", got, n)
	}
	// nil root（未装配）⇒ 恒等，不 panic。
	if got := (&moveFileCtx{}).toCharge(n); got != n {
		t.Fatalf("nil root 必须恒等: got=%d want=%d", got, n)
	}
}
