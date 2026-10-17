// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

// quota_volume_user_test.go —— `QuotaVolume` 的**每用户读侧**（片 5：用户配额**按卷持有**）。
//
// 设计口径（docs/designs/2026-10-16-quota-volume-layer.md §1.3/§1.5）：`QuotaVolume` 是存储卷的
// **唯一配额权威**，每用户配额**按卷持有**；用户视图 = 对「该用户可访问的卷」**求和**（联邦查询 ✓）。
// ⇒ 本文件钉住「**单卷**读侧」；跨卷求和与 ACL 收敛在 `pkg/server` 侧用例。
//
// **判别力设计** ✓：
//   - `ok=false` 必须与「0 用量」可区分 —— 未知 owner / 未装配 resolver / nil 接收者都返回
//     `(0, false)`（**禁当 0** ✗）；若实现退化为「恒 (x, true)」⇒ 用例红；
//   - 用**两个不同 owner 各自独立 Scope**（`alice`/`bob`）⇒ 若实现「任意 owner 都读同一 Scope」
//     ⇒ 隔离断言红。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// userScopeResolver 按 owner 返回独立 Scope（未登记 owner ⇒ nil = 无每用户权威）。
func userScopeResolver(scopes map[string]*quota.Scope) func(string) *quota.Scope {
	return func(owner string) *quota.Scope { return scopes[owner] }
}

// TestQuotaVolume_UserUsagePerVolume 钉住每用户用量**按卷**读取（节点级 ⇒ 该 owner 全子树）。
func TestQuotaVolume_UserUsagePerVolume(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1 << 20)
	alice := quota.NewPool(1<<20).Scope("/tenant/alice", 1<<20)
	bob := quota.NewPool(1<<20).Scope("/tenant/bob", 1<<20)
	qv := NewQuotaVolume(bareQuotaInner{name: "v1"}, userScopeResolver(map[string]*quota.Scope{
		"alice": alice,
		"bob":   bob,
	}), pool, nil, nil)

	// ① 初始：两用户各自 0（**有权威** ⇒ ok=true）。
	if used, ok := qv.UserUsage("alice"); !ok || used != 0 {
		t.Fatalf("alice 初始: used=%d ok=%v，want 0/true", used, ok)
	}
	if used, ok := qv.UserUsage("bob"); !ok || used != 0 {
		t.Fatalf("bob 初始: used=%d ok=%v，want 0/true", used, ok)
	}

	// ② 只给 alice 记账 ⇒ alice 变、bob **不受影响**（**每用户隔离**：若实现读同一 Scope 则红）。
	alice.Adjust(0, 8192)
	if used, ok := qv.UserUsage("alice"); !ok || used != 8192 {
		t.Fatalf("alice 记账后: used=%d ok=%v，want 8192/true", used, ok)
	}
	if used, ok := qv.UserUsage("bob"); !ok || used != 0 {
		t.Fatalf("bob 应不受影响: used=%d ok=%v，want 0/true（每用户隔离）", used, ok)
	}

	// ③ 覆盖写差分（旧值释放）⇒ 读侧跟随（**禁双计**）。
	alice.Adjust(8192, 20480)
	if used, ok := qv.UserUsage("alice"); !ok || used != 20480 {
		t.Fatalf("覆盖写后: used=%d ok=%v，want 20480/true（旧值须释放）", used, ok)
	}
}

// TestQuotaVolume_UserUsageUnknownOwnerIsNotZero 钉住「**未知 ≠ 0**」：无每用户权威必须
// `ok=false`（禁把「不知道」当「零用量」✗ —— 联邦求和会因此静默少算）。
func TestQuotaVolume_UserUsageUnknownOwnerIsNotZero(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1 << 20)
	qv := NewQuotaVolume(bareQuotaInner{name: "v1"}, userScopeResolver(map[string]*quota.Scope{}), pool, nil, nil)

	used, ok := qv.UserUsage("nobody")
	if ok {
		t.Fatalf("未知 owner 必须 ok=false（禁当 0）: used=%d", used)
	}
	if used != 0 {
		t.Fatalf("未知 owner used=%d，want 0（值可为 0，但 ok 必须 false）", used)
	}
	if capBytes, ok := qv.UserCapacity("nobody"); ok || capBytes != 0 {
		t.Fatalf("未知 owner 上限: cap=%d ok=%v，want 0/false", capBytes, ok)
	}
}

// TestQuotaVolume_UserUsageNilAndNoResolver 钉住「**未装配 ≠ 无限制**」：nil 接收者与未装配
// resolver 都返回 `(0, false)`（与 `VolumeUsed` 同型 ✓）。
func TestQuotaVolume_UserUsageNilAndNoResolver(t *testing.T) {
	t.Parallel()
	var nilQV *QuotaVolume
	if used, ok := nilQV.UserUsage("alice"); ok || used != 0 {
		t.Fatalf("nil QuotaVolume: used=%d ok=%v，want 0/false", used, ok)
	}
	if capBytes, ok := nilQV.UserCapacity("alice"); ok || capBytes != 0 {
		t.Fatalf("nil QuotaVolume 上限: cap=%d ok=%v，want 0/false", capBytes, ok)
	}

	// 未装配 resolver（nil）⇒ 无每用户权威（**不是**「无限制」✗）。
	qv := NewQuotaVolume(bareQuotaInner{name: "v1"}, nil, quota.NewPool(1<<20), nil, nil)
	if used, ok := qv.UserUsage("alice"); ok || used != 0 {
		t.Fatalf("无 resolver: used=%d ok=%v，want 0/false", used, ok)
	}
}

// TestQuotaVolume_UserCapacityPerVolume 钉住每用户上限**按卷**读取（Scope 的 maxBytes），
// 且运行期改额后读侧跟随（配额调整无需重建卷 ✓）。
func TestQuotaVolume_UserCapacityPerVolume(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1 << 20)
	alice := quota.NewPool(1<<20).Scope("/tenant/alice", 4096)
	qv := NewQuotaVolume(bareQuotaInner{name: "v1"}, userScopeResolver(map[string]*quota.Scope{
		"alice": alice,
	}), pool, nil, nil)

	if got, ok := qv.UserCapacity("alice"); !ok || got != 4096 {
		t.Fatalf("alice 每用户上限: cap=%d ok=%v，want 4096/true", got, ok)
	}
	alice.Pool().SetMaxBytes(8192)
	if got, ok := qv.UserCapacity("alice"); !ok || got != 8192 {
		t.Fatalf("改额后: cap=%d ok=%v，want 8192/true", got, ok)
	}
}
