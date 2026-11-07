// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// release_charge_for_test.go 钉住装配层的「同源释放金额捕获」入口
// （独立复审「释放≠结算」类根治，2026-10-18）。
//
// 背景：pkg/server 的版本删除/回收清理此前直接用 `root.Stat` 尺寸释放 ⇒ 对 `footprint ≠ 公式`
// 的卷（secretdata 的 parity + meta pad）**少释放** ⇒ 幽灵占用/假 507。修法：暴露
// `ReleaseChargeFor`（卷自报实测足迹优先，能力缺失回落 fallback），并要求调用方在**删除/改名
// 之前**调用（对象存在 ⇒ 足迹可测）。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseChargeFor_PrefersVolumeFootprint(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	const footprint = int64(7000)
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant 不可用")
	}
	env.phys = trashFootprintStub{foot: footprint}
	env.rebuild()

	// ① 有足迹能力 ⇒ 必须取**实测足迹**（不是 fallback 的逻辑长度 5）。
	if got := env.svc.ReleaseChargeFor(tnt, "alice", "user/a.txt", 5); got != footprint {
		t.Fatalf("ReleaseChargeFor=%d want %d（卷自报足迹优先；未修必红：用 Stat 尺寸 ⇒ 少释放）",
			got, footprint)
	}
	// ② 无足迹能力 ⇒ 回落 fallback（明文/加密本地卷两口径本就一致 ⇒ 零回归）。
	env.phys = nil
	env.rebuild()
	if got := env.svc.ReleaseChargeFor(tnt, "alice", "user/a.txt", 5); got != 5 {
		t.Fatalf("无足迹能力应回落 fallback(5)，实际 %d", got)
	}
	// ③ fallback <= 0 且无能力 ⇒ 恒等回落（size<=0 的空操作语义由调用方保证）。
	if got := env.svc.ReleaseChargeFor(tnt, "alice", "user/a.txt", 0); got != 0 {
		t.Fatalf("fallback 0 应回落 0，实际 %d", got)
	}
	_ = os.MkdirAll(filepath.Join(t.TempDir(), "x"), 0o755) // 保持 import 使用（os/filepath 由其它用例共享）
}
