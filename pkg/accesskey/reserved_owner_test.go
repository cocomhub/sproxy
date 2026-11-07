// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"errors"
	"testing"
)

// reserved_owner_test.go 锁定 M1-9（设计 docs/designs/2026-10-19-audit-service.md §5.6）：
// **保留租户名**（审计租户 `audit`）不得被用户面登记占用 —— 租户键的唯一收口点是
// `UpsertAK`（注册 / admin 新增凭据 / 改名均经此），故在此 fail-closed。

func TestIsReservedOwner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		owner string
		want  bool
	}{
		{"audit", true},
		{"Audit", false}, // 大小写敏感：租户名按原样比较（存储层同口径）
		{"alice", false},
		{"", false}, // 空值不算保留（由调用方按各自语义拒绝空值）
		{"audit2", false},
	} {
		if got := IsReservedOwner(tc.owner); got != tc.want {
			t.Errorf("IsReservedOwner(%q) = %v, want %v", tc.owner, got, tc.want)
		}
	}
}

// TestUpsertAK_RejectsReservedOwner 断言保留租户名被拒且**无副作用**（fail-closed：
// 拒绝后 ring 中不得出现该 AK —— 只拒绝不入账，否则仍是「占用」）。
func TestUpsertAK_RejectsReservedOwner(t *testing.T) {
	t.Parallel()
	r := NewRing()
	err := r.UpsertAK("ak-0123456789abcdef0123456789abcdef", "audit")
	if !errors.Is(err, ErrReservedOwner) {
		t.Fatalf("保留租户名应返回 ErrReservedOwner, got %v", err)
	}
	if _, ok := r.GetKey("ak-0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("拒绝后不得在 ring 中留下该 AK（无副作用）")
	}
	if r.Len() != 0 {
		t.Fatalf("ring 应保持空, got %d", r.Len())
	}
}

// TestUpsertAK_NormalOwnerStillWorks 零回归：普通租户名照常登记/更新。
func TestUpsertAK_NormalOwnerStillWorks(t *testing.T) {
	t.Parallel()
	r := NewRing()
	const ak = "ak-0123456789abcdef0123456789abcdef"
	if err := r.UpsertAK(ak, "alice"); err != nil {
		t.Fatalf("普通租户名应可登记, got %v", err)
	}
	if err := r.UpsertAK(ak, "bob"); err != nil {
		t.Fatalf("已存在 AK 应可更新 Owner, got %v", err)
	}
	k, ok := r.GetKey(ak)
	if !ok || k.Owner != "bob" {
		t.Fatalf("Owner 应更新为 bob, got %+v ok=%v", k, ok)
	}
	// 已存在的 AK 改名到保留名同样必须被拒（不得绕过收口）。
	if err := r.UpsertAK(ak, "audit"); !errors.Is(err, ErrReservedOwner) {
		t.Fatalf("改名到保留名应被拒, got %v", err)
	}
	if k, _ := r.GetKey(ak); k.Owner != "bob" {
		t.Fatalf("被拒改名不得生效, got owner=%q", k.Owner)
	}
}
