// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package trusted

// persist_calc_meta_invalidate_test.go 钉住「`persistCalcMeta` 失败必须失效旧 sidecar」
// （独立审核 P1，2026-10-18）。
//
// `persistCalcMeta` 是 `WriteIfAbsent` 与 `writeMetaAfter` 共用的 meta 落盘路径；它有三处失败
// 出口（Validate 失败 / Marshal 失败 / meta 写失败），此前**全部只 return / Warn** ✗。而
// `WriteFile` 的 C1 修复早已在同类失败时删旧 sidecar，注释自陈理由：
//
//	残留描述**旧内容**的陈旧 meta 会使读路径 FileMeta 成功解析旧哈希、按旧分块校验新内容
//	恒失配（硬失败，与 missing（读侧不校验）不同）⇒ **文件被固化不可读**。
//
// ⇒ 两条路径必须对称。本用例走**第三处出口**（meta 写失败，可用 `failMetaFS` 确定性注入）：
// 先造「主文件已删、陈旧 sidecar 残留」的孤儿状态，再 `WriteIfAbsent` 写入新内容 ⇒ 主写成功、
// meta 落盘失败 ⇒ 陈旧 sidecar 必须被失效（退化为 missing = 读侧不校验，下次覆盖写自愈）。

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestTrustedVolumeFS_WriteIfAbsentMetaFailureInvalidatesStaleSidecar meta 落盘失败时，
// 陈旧 sidecar 必须被失效（与 WriteFile 的 C1 修复对称）。
func TestTrustedVolumeFS_WriteIfAbsentMetaFailureInvalidatesStaleSidecar(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	tv := Wrap(inner, Options{}).(*TrustedVolumeFS)
	ctx := context.Background()

	// 先正常写一次（产生 sidecar），再只删**主文件** ⇒ 留下孤儿陈旧 sidecar。
	if err := tv.WriteFile(ctx, syncpkg.LogicalKey{}, "user/a.bin", strings.NewReader("v1-content"), 10, 0); err != nil {
		t.Fatalf("WriteFile(v1): %v", err)
	}
	if err := inner.Delete(ctx, "user/a.bin"); err != nil {
		t.Fatalf("Delete(主文件): %v", err)
	}
	// 确保 sidecar 确实存在（前置；否则本用例无判别力）。
	// 用 Stat 而非 OpenRead：后者会**持句柄**，Windows 下锁住文件会让后续 Delete 失败。
	if ent, serr := inner.Stat(ctx, meta.MetaPath("user/a.bin")); serr != nil || ent == nil {
		t.Fatalf("前置失败：孤儿 sidecar 不存在（err=%v ent=%v）", serr, ent)
	}

	// 注入 meta 写失败 ⇒ persistCalcMeta 的第三处失败出口。
	fail := &failMetaFS{LocalFS: inner}
	tvF := Wrap(fail, Options{}).(*TrustedVolumeFS)
	written, err := tvF.WriteIfAbsent(ctx, "user/a.bin", strings.NewReader("v2-content"), 10, 0)
	if err != nil {
		t.Fatalf("meta 落盘失败不得失败主写: %v", err)
	}
	if !written {
		t.Fatal("目标不存在 ⇒ WriteIfAbsent 必须实际写入（written=true）")
	}
	// 主文件已是新内容（主写未被 meta 失败拖累）。
	rc, rerr := inner.OpenRead(ctx, "user/a.bin")
	if rerr != nil {
		t.Fatalf("主文件不可读: %v", rerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "v2-content" {
		t.Fatalf("主文件 = %q, want v2-content", got)
	}
	// **核心断言**：陈旧 sidecar 必须被失效（未修必红：persistCalcMeta 只 Warn ⇒ 残留陈旧
	// meta ⇒ 读路径按旧分块校验新内容恒失配 ⇒ 文件固化不可读）。
	if ent, serr := inner.Stat(ctx, meta.MetaPath("user/a.bin")); serr == nil && ent != nil {
		t.Fatal("persistCalcMeta 失败时旧陈旧 sidecar 必须被失效（未修必红）")
	}
}
