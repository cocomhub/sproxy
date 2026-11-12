// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"strings"
	"testing"
)

// TestAllVerified_全绿才通过 锁定用户裁定（2026-11-12）：
// 批量路径的"成功"必须 = **全部一致**；任一条非 VerifyOK ⇒ 必须失败。
// 目的是**绝不允许出现「批量里有一条不一致却整体返回成功」**（使用方拿到成功却带着坏 md5）。
func TestAllVerified_全绿才通过(t *testing.T) {
	t.Parallel()
	ok := func(k string) VerifyResult {
		return VerifyResult{Item: VerifyItem{Key: k, MD5: "aa"}, State: VerifyOK}
	}

	// ① 全部 OK ⇒ nil
	if err := AllVerified([]VerifyResult{ok("a"), ok("b")}, 2); err != nil {
		t.Fatalf("全绿应通过，got %v", err)
	}

	// ② 有一条 Mismatch ⇒ 必须失败，且错误里要点名该 key
	res := []VerifyResult{ok("a"), {Item: VerifyItem{Key: "b"}, State: VerifyMismatch, Reason: "远端 md5 不一致"}}
	err := AllVerified(res, 2)
	if err == nil {
		t.Fatal("存在 mismatch 必须失败（绝不能整体成功）")
	}
	if !strings.Contains(err.Error(), "b") || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("错误应点名具体项与状态: %v", err)
	}

	// ③ VerifyInvisible（不可见）同样不得视为成功
	if err := AllVerified([]VerifyResult{ok("a"), {Item: VerifyItem{Key: "b"}, State: VerifyInvisible}}, 2); err == nil {
		t.Fatal("invisible 不得视为成功")
	}

	// ④ 结果条数与期望不符（后端漏返回）⇒ 视为**未校验**，必须失败
	if err := AllVerified([]VerifyResult{ok("a")}, 2); err == nil {
		t.Fatal("漏返回（条数不符）必须失败，否则等于放弃校验")
	}
	// ⑤ 期望 0 条且无结果 ⇒ 通过（边界）
	if err := AllVerified(nil, 0); err != nil {
		t.Fatalf("空批量应通过: %v", err)
	}
}

// TestAllVerified_未知状态不得放过 锁定：任何非 VerifyOK 的（含未来新增态）都必须失败——
// 白名单式判定，避免"新状态没被识别 ⇒ 静默通过"。
func TestAllVerified_未知状态不得放过(t *testing.T) {
	t.Parallel()
	err := AllVerified([]VerifyResult{{Item: VerifyItem{Key: "x"}, State: VerifyState("weird")}}, 1)
	if err == nil {
		t.Fatal("未知状态不得放过（应白名单式只认 VerifyOK）")
	}
}
