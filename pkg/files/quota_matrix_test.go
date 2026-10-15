// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// quota_matrix_test.go 是配额 v3 的**对账矩阵**（规格 §10 S2 验收）：三种记账口径的卷型 ×
// 操作序列，**每步**断言「owner 配额用量 == 该步结束时的应记账额」。
//
// 为什么需要矩阵：本项目的「幽灵占用 / 配额漂移」缺陷**反复**出现在「某个操作漏接记账或
// 释放」（写、覆盖写、复制、改名、软删、回收站各出现过一次）；单点用例只覆盖自己那一步
// ⇒ 漏接的那步无人发现。矩阵把「每步之后账本必须自洽」变成显式断言 ⇒ 任何新操作漏接即红。
//
// 三种口径（写侧结算值）：
//   - 明文本地 = 逻辑长度；
//   - 加密本地 = `storage.EncryptedPhysicalSize(逻辑)`（每 64KiB +32B + 22B 头）；
//   - 卷自报足迹 = 卷能力返回的实测足迹（secretdata 的 meta pad/parity 无法由逻辑推出）。
func TestQuotaLedgerMatrix_WriteOverwriteRenameDelete(t *testing.T) {
	t.Parallel()
	const foot = int64(3 << 20)
	flavors := []struct {
		name   string
		env    func(*testing.T) *dirsEnv
		charge func(int64) int64
	}{
		{"明文本地", writeTargetEnv, func(n int64) int64 { return n }},
		{"加密本地", encryptedEnv, storage.EncryptedPhysicalSize},
		{"卷自报足迹", func(t *testing.T) *dirsEnv {
			t.Helper()
			e := writeTargetEnv(t)
			// root 非 nil ⇒ 替身按「对象是否存在」报告足迹（与生产语义同构）。
			e.svc.rt.phys = fakePhysFootprint{foot: foot, root: e.tenantFor("alice").Root()}
			return e
		}, func(int64) int64 { return foot }},
	}
	for _, f := range flavors {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			e := f.env(t)
			ctx := context.Background()
			sc := e.quotaScopeFor("alice", "user/m.bin")
			if sc == nil {
				t.Fatal("owner Scope 不可用")
			}
			// assertUsage 断言「本步之后账本自洽」（差值为正 = 幽灵占用；为负 = 少记）。
			assertUsage := func(step string, want int64) {
				t.Helper()
				if got := sc.Usage(); got != want {
					t.Fatalf("%s 后 owner 配额=%d want %d（差 %+d）", step, got, want, got-want)
				}
			}

			// 1) 写入 ⇒ 等于该卷型口径的应记账额。
			p1 := bytes.Repeat([]byte("a"), 100_000) // 跨 2 个 64KiB 块（加密口径可区分）
			if _, err := e.svc.WriteTarget(ctx, WriteTargetInput{
				Owner: "alice", RemotePath: "m.bin", ClientSize: int64(len(p1)),
			}, bytes.NewReader(p1)); err != nil {
				t.Fatalf("write: %v", err)
			}
			assertUsage("write", f.charge(int64(len(p1))))

			// 2) 覆盖写（更大）⇒ 按**新**尺寸记账（不是累加，也不是残留旧值）。
			p2 := bytes.Repeat([]byte("b"), 150_000)
			if _, err := e.svc.WriteTarget(ctx, WriteTargetInput{
				Owner: "alice", RemotePath: "m.bin", ClientSize: int64(len(p2)),
			}, bytes.NewReader(p2)); err != nil {
				t.Fatalf("overwrite: %v", err)
			}
			assertUsage("overwrite", f.charge(int64(len(p2))))

			// 3) 改名 ⇒ 内容零改动 ⇒ 记账不变（改名只重写元数据，不得多记/少记）。
			if _, err := e.svc.RenameFile(ctx, RenameFileInput{
				Owner: "alice", From: "m.bin", To: "n.bin",
				ExpectedChecksum: testutil.SHA256Hex(p2),
			}); err != nil {
				t.Fatalf("rename: %v", err)
			}
			assertUsage("rename", f.charge(int64(len(p2))))

			// 4) 硬删 ⇒ 释放**同口径**金额 ⇒ 归零（少释放 = 幽灵占用/假 507）。
			if _, err := e.svc.DeleteFile(ctx, DeleteFileInput{
				Owner: "alice", RemotePath: "n.bin", SkipChecksumVerify: true,
			}); err != nil {
				t.Fatalf("hard delete: %v", err)
			}
			assertUsage("hard delete", 0)
		})
	}
}
