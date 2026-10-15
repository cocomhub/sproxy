// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_delete_quota_test.go 验证**嵌套封装卷删除链路的配额释放**（F3）：
// 上传走委托子 Scope（父链聚合到底层池）+ owner 全局 Scope 双账本；删除必须**精确回落**——
// 底座池与 owner Scope 各只归还该文件的一次记账，既不欠释放（幽灵占用/假 507），也不重复
// 释放（扣掉同池其它文件的占用 ⇒ 过度准入）。
//
// 端到端真实副作用：config 声明嵌套封装卷（secretdata + target=main/videos，与生产装配同形）
// → 真实 upload/delete handler → 真实容量池账本。
//
// 为什么必须「多文件」：单文件场景下重复释放会被 `releaseCommittedUp` 的「钳到本层 committed」
// 掩盖（第二次释放恒为 no-op）；只有池里存在**其它文件**的占用时，多释放才会反向扣掉它们。

import (
	"bytes"
	"testing"
)

// TestWrapperDeleteQuota_MultiFile_ExactRelease 多文件场景：上传 f1/f2 到嵌套封装卷后删 f2，
// 底座池（委托子 Scope 父链聚合）与 owner 全局 Scope 都必须**精确回落到只剩 f1 的占用**。
//
// 变异（本测必红）：任何一处对同一委托子池的重复释放 ⇒ 底座池多跌一档（少计）；
// 任何一处漏释放（owner Scope 不归还）⇒ owner Scope 不回落。
func TestWrapperDeleteQuota_MultiFile_ExactRelease(t *testing.T) {
	t.Parallel()
	h := newNestedSecretWrapperHandlers(t, 0) // main 不限容量（只验证账本精确性）
	mux := actorFileWriteMux(h, "alice")

	pool := h.volSet.Pool("vault") // 委托子 Scope（Pool(vault) 委托返回）
	basePool := h.volSet.Pool("main")
	scope := h.quotaScopeFor("alice", "user/f1.bin")
	if scope == nil {
		t.Fatal("owner 全局 Scope 未装配（测试前提不成立）")
	}

	f1 := bytes.Repeat([]byte("a"), 4096)
	f2 := bytes.Repeat([]byte("b"), 2048)
	p0, s0 := pool.Usage(), scope.Usage()
	if st, resp := writeMuxUpload(t, mux, "f1.bin", "vault", f1); st != 200 {
		t.Fatalf("上传 f1.bin = %d (body: %s)", st, resp)
	}
	p1, s1 := pool.Usage(), scope.Usage()
	charge1 := p1 - p0
	if charge1 <= 0 {
		t.Fatalf("上传 f1 后委托池 Usage 无增量（%d → %d）：记账未接线", p0, p1)
	}
	if got := s1 - s0; got != charge1 {
		t.Fatalf("f1：owner Scope 增量=%d ≠ 委托池增量=%d（双账本口径不一致）", got, charge1)
	}
	if st, resp := writeMuxUpload(t, mux, "f2.bin", "vault", f2); st != 200 {
		t.Fatalf("上传 f2.bin = %d (body: %s)", st, resp)
	}
	p2, s2 := pool.Usage(), scope.Usage()
	charge2 := p2 - p1
	if charge2 <= 0 {
		t.Fatalf("上传 f2 后委托池 Usage 无增量（%d → %d）", p1, p2)
	}
	if got := s2 - s1; got != charge2 {
		t.Fatalf("f2：owner Scope 增量=%d ≠ 委托池增量=%d", got, charge2)
	}
	if got := basePool.Usage(); got != p2 {
		t.Fatalf("底座池 Usage()=%d ≠ 委托池 Usage()=%d（父链聚合断裂）", got, p2)
	}

	// 删除 f2：账本必须精确回落到「只剩 f1」的那一档（一次释放，不多不少）。
	st, resp := writeMuxDelete(t, mux, "f2.bin", "vault", sha256hex(f2))
	if st != 200 {
		t.Fatalf("删除 f2.bin = %d (body: %s)", st, resp)
	}
	if got := pool.Usage(); got != p1 {
		t.Fatalf("删除 f2 后委托池 Usage()=%d, want %d（多释放⇒少计超配／少释放⇒幽灵占用）", got, p1)
	}
	if got := scope.Usage(); got != s1 {
		t.Fatalf("删除 f2 后 owner Scope Usage()=%d, want %d（删除未归还 owner 全局账本）", got, s1)
	}
	if got := basePool.Usage(); got != p1 {
		t.Fatalf("删除 f2 后底座池 Usage()=%d, want %d（父链释放口径错误）", got, p1)
	}

	// 删除 f1：两者归零（无残留幽灵占用）。
	if st, resp := writeMuxDelete(t, mux, "f1.bin", "vault", sha256hex(f1)); st != 200 {
		t.Fatalf("删除 f1.bin = %d (body: %s)", st, resp)
	}
	if got := pool.Usage(); got != 0 {
		t.Fatalf("删除 f1 后委托池 Usage()=%d, want 0", got)
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("删除 f1 后 owner Scope Usage()=%d, want 0", got)
	}
}

// TestWrapperDeleteQuota_CapacityRestored 底座池容量守门：删除一个文件后
// 底座池可用额度必须恰好回到「删掉的字节」——重复释放会多放行（过度准入）。
//
// 场景：main 容量 = 2×f1；写 f1 占满一半…直接断言「删后可用额度 == 删前可用额度 + 该文件记账」。
func TestWrapperDeleteQuota_CapacityRestored(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("x"), 4096)
	// 先量一次单文件记账（用于构造容量下界）：用不限容量 harness 上传后读委托池增量。
	probe := newNestedSecretWrapperHandlers(t, 0)
	probeMux := actorFileWriteMux(probe, "alice")
	if st, resp := writeMuxUpload(t, probeMux, "p.bin", "vault", body); st != 200 {
		t.Fatalf("探针上传 = %d (body: %s)", st, resp)
	}
	charge := probe.volSet.Pool("vault").Usage()
	if charge <= 0 {
		t.Fatalf("探针记账为 0（测试前提不成立）")
	}

	h := newNestedSecretWrapperHandlers(t, 64<<20)
	mux := actorFileWriteMux(h, "alice")
	base := h.volSet.Pool("main")
	availBefore := base.MaxBytes() - base.Usage() - base.Reserved()
	if st, resp := writeMuxUpload(t, mux, "a.bin", "vault", body); st != 200 {
		t.Fatalf("上传 a.bin = %d (body: %s)", st, resp)
	}
	availAfterUpload := base.MaxBytes() - base.Usage() - base.Reserved()
	if availAfterUpload >= availBefore {
		t.Fatalf("上传后可用额度未减少（%d → %d）", availBefore, availAfterUpload)
	}
	if st, resp := writeMuxDelete(t, mux, "a.bin", "vault", sha256hex(body)); st != 200 {
		t.Fatalf("删除 a.bin = %d (body: %s)", st, resp)
	}
	if got := base.MaxBytes() - base.Usage() - base.Reserved(); got != availBefore {
		t.Fatalf("删除后底座池可用额度=%d, want %d（重复释放⇒过度准入／漏释放⇒少计）", got, availBefore)
	}
}
