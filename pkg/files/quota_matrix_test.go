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

// TestQuotaLedgerMatrix_CopyTrashDir 扩展对账矩阵：复制 / 回收站 / 目录足迹三组操作，
// 每组**每步**断言账本自洽（与上一个矩阵同口径：差值为正 = 幽灵占用，为负 = 少记）。
//
// 覆盖的缺陷类（每一条在本项目历史上都出现过）：
//   - 复制漏记账（同卷复制 = 新对象 ⇒ 全额计；去重关闭时不得共享占用）；
//   - 软删**误释放**（回收站保留 ⇒ 释放会让用户「删了就有空间」但文件还在）；
//   - 清空回收站**漏释放**（trash 条目删除时释放口径不同源 ⇒ 幽灵占用）；
//   - 目录 meta 足迹（mkdir 收、rmdir 放，含祖先链差分）。
func TestQuotaLedgerMatrix_CopyTrashDir(t *testing.T) {
	t.Parallel()
	const foot = int64(2 << 20)
	const dirFoot = int64(1 << 20)
	flavors := []struct {
		name   string
		env    func(*testing.T) *dirsEnv
		charge func(int64) int64
	}{
		{"明文本地", writeTargetEnv, func(n int64) int64 { return n }},
		{"卷自报足迹", func(t *testing.T) *dirsEnv {
			t.Helper()
			e := writeTargetEnv(t)
			e.svc.rt.phys = fakePhysFootprint{foot: foot, dirFoot: dirFoot, root: e.tenantFor("alice").Root()}
			return e
		}, func(int64) int64 { return foot }},
	}
	for _, f := range flavors {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			e := f.env(t)
			ctx := context.Background()
			sc := e.quotaScopeFor("alice", "user/a.bin")
			if sc == nil {
				t.Fatal("owner Scope 不可用")
			}
			assertUsage := func(step string, want int64) {
				t.Helper()
				if got := sc.Usage(); got != want {
					t.Fatalf("%s 后 owner 配额=%d want %d（差 %+d）", step, got, want, got-want)
				}
			}

			// 1) 写入 a ⇒ charge
			payload := bytes.Repeat([]byte("a"), 64_000)
			if _, err := e.svc.WriteTarget(ctx, WriteTargetInput{
				Owner: "alice", RemotePath: "a.bin", ClientSize: int64(len(payload)),
			}, bytes.NewReader(payload)); err != nil {
				t.Fatalf("write: %v", err)
			}
			one := f.charge(int64(len(payload)))
			assertUsage("write", one)

			// 2) 同卷复制 a→b ⇒ 新对象全额计（去重关闭）
			if _, err := e.svc.CopyFile(ctx, CopyFileInput{Owner: "alice", From: "a.bin", To: "b.bin"}); err != nil {
				t.Fatalf("copy: %v", err)
			}
			assertUsage("copy", 2*one)

			// 3) 软删 + 清空回收站（**仅本地卷型**）：
			//    回收站是本地卷能力（trash 桶在本地租户根）。软删 ⇒ 保留 ⇒ 记账**不变**
			//    （若释放，用户会「删了就有空间」而文件还在）；清空回收站 ⇒ 真正消失 ⇒ 释放。
			//    注：卷自报足迹卷在**生产中**必然是外部卷 ⇒ 无本地回收站、软删 **fail-closed**
			//    （禁静默降级为不可恢复硬删，见 `delete_soft_external_test.go`）；而「本地路由 +
			//    足迹能力」是不存在的组合 ⇒ 本用例不构造它（避免用不真实的夹具断言错误契约）。
			if f.name != "卷自报足迹" {
				if _, err := e.svc.DeleteFile(ctx, DeleteFileInput{
					Owner: "alice", RemotePath: "a.bin", SkipChecksumVerify: true, SoftDelete: true,
				}); err != nil {
					t.Fatalf("本地卷软删应成功: %v", err)
				}
				assertUsage("soft delete", 2*one)
				if err := e.svc.EmptyTrash(ctx, "alice"); err != nil {
					t.Fatalf("empty trash: %v", err)
				}
				assertUsage("empty trash", one)
			}

			// 5) 硬删 b：本地卷型此时只剩 b（a 已随回收站清空）⇒ 归零；卷自报足迹卷型跳过了
			//    回收站步骤 ⇒ a 仍在 ⇒ 应剩 one（顺带覆盖「非最后对象释放」路径）。
			if _, err := e.svc.DeleteFile(ctx, DeleteFileInput{
				Owner: "alice", RemotePath: "b.bin", SkipChecksumVerify: true,
			}); err != nil {
				t.Fatalf("hard delete: %v", err)
			}
			wantAfterDelete := int64(0)
			if f.name == "卷自报足迹" {
				wantAfterDelete = one
			}
			assertUsage("hard delete", wantAfterDelete)
			if f.name == "卷自报足迹" {
				// 收尾：删掉最后一个对象 ⇒ 必须归零（覆盖「最后对象释放」路径）。
				if _, err := e.svc.DeleteFile(ctx, DeleteFileInput{
					Owner: "alice", RemotePath: "a.bin", SkipChecksumVerify: true,
				}); err != nil {
					t.Fatalf("hard delete a: %v", err)
				}
				assertUsage("hard delete a（最后一个）", 0)
			}

			// 6) 目录足迹：mkdir 收（含祖先链差分）、rmdir 放 ⇒ 回到 0
			if _, err := e.svc.MakeDir(ctx, "alice", "videos", ""); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			wantAfterMkdir := int64(0)
			if f.name == "卷自报足迹" {
				wantAfterMkdir = dirFoot // 该卷型目录 meta 也按物理足迹计
			}
			assertUsage("mkdir", wantAfterMkdir)
			if _, err := e.svc.RemoveDir(ctx, "alice", "videos", "", true); err != nil {
				t.Fatalf("rmdir: %v", err)
			}
			assertUsage("rmdir", 0)
		})
	}
}
