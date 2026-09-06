// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestRoleConstants 验证 Role 枚举常量取值（DEC-A）。
func TestRoleConstants(t *testing.T) {
	if RoleUser != "user" {
		t.Errorf("RoleUser = %q, want %q", RoleUser, "user")
	}
	if RoleNode != "node" {
		t.Errorf("RoleNode = %q, want %q", RoleNode, "node")
	}
	if RoleAdmin != "admin" {
		t.Errorf("RoleAdmin = %q, want %q", RoleAdmin, "admin")
	}
}

// TestRing_GetKey 验证 GetKey 返回含 Role 的 Key 深拷贝（I1）：
// AddKey 过的 AK 返回副本（含 Role）；修改返回值不影响原 ring；不存在 → false。
func TestRing_GetKey(t *testing.T) {
	r := NewRing()
	ak := "ak-1122334455667788"
	if err := r.UpsertAK(ak, "owner-1"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	if _, err := r.AddKey(ak, must32BHex(t, 0x11), WithID("skey-0000000000aa")); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	// 同包测试直接置内部 Role/TOTPSecret。
	r.m[ak].Role = RoleNode
	r.m[ak].TOTPSecret = []byte{0xde, 0xad, 0xbe, 0xef}

	got, ok := r.GetKey(ak)
	if !ok {
		t.Fatalf("GetKey(已登记 AK) 应 ok=true")
	}
	if got == nil {
		t.Fatalf("GetKey 应返回非 nil Key")
	}
	if got.Role != RoleNode {
		t.Errorf("GetKey 应携带 Role, got %q, want %q", got.Role, RoleNode)
	}
	if got.Owner != "owner-1" {
		t.Errorf("GetKey Owner 应为 owner-1, got %q", got.Owner)
	}
	if !bytes.Equal(got.TOTPSecret, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("GetKey 应携带 TOTPSecret")
	}
	if len(got.Entries) != 1 || len(got.Entries[0].SK) != 32 {
		t.Fatalf("GetKey 应携带 SK 条目（32B SK）")
	}

	// 深拷贝：修改返回值不影响原 ring（Role / SK / TOTPSecret）。
	got.Role = RoleAdmin
	got.Entries[0].SK[0] ^= 0xff
	got.TOTPSecret[0] ^= 0xff
	if k2, _ := r.GetKey(ak); k2.Role != RoleNode {
		t.Errorf("GetKey 深拷贝失败：修改返回值 Role 影响 ring")
	}
	if bytes.Equal(r.m[ak].Entries[0].SK, got.Entries[0].SK) {
		t.Errorf("GetKey 深拷贝失败：修改返回值 SK 影响 ring")
	}
	if r.m[ak].TOTPSecret[0] == got.TOTPSecret[0] {
		t.Errorf("GetKey 深拷贝失败：修改返回值 TOTPSecret 影响 ring")
	}

	// 不存在 AK → (nil, false)。
	if k, ok := r.GetKey("ak-9999999999999999"); ok || k != nil {
		t.Errorf("GetKey(不存在) 应返回 (nil, false), got (%v, %v)", k, ok)
	}
}

// TestRing_Snapshot_PreservesRole cloneKey 复制 Role/TOTPSecret：Snapshot 保留账号级字段。
func TestRing_Snapshot_PreservesRole(t *testing.T) {
	r := NewRing()
	ak := "ak-4433221100112233"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	r.m[ak].Role = RoleNode
	r.m[ak].TOTPSecret = []byte{1, 2, 3, 4}
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot 长度 = %d, want 1", len(snap))
	}
	if snap[0].Role != RoleNode {
		t.Errorf("Snapshot 应保留 Role, got %q", snap[0].Role)
	}
	if !bytes.Equal(snap[0].TOTPSecret, []byte{1, 2, 3, 4}) {
		t.Errorf("Snapshot 应保留 TOTPSecret")
	}
	// 深拷贝：改 Snapshot 不影响 ring。
	snap[0].Role = RoleAdmin
	snap[0].TOTPSecret[0] = 0xEE
	if r.m[ak].Role != RoleNode || r.m[ak].TOTPSecret[0] != 1 {
		t.Errorf("Snapshot 未深拷贝 Role/TOTPSecret")
	}
}

// TestRing_Replace_NormalizesEmptyRole 旧 credentials.json（4A 无 role 字段）载入时
// Role 空值归一为 RoleUser（R3-M4）；显式 role 字段保留。
func TestRing_Replace_NormalizesEmptyRole(t *testing.T) {
	old := []Key{
		{AK: "ak-old-1234567890abcdef", Owner: "o", Entries: []SKEntry{
			{ID: "skey-000000000001", SK: must32BHex(t, 0x01), Status: StatusActive},
		}},
	}
	r := NewRing()
	if err := r.Replace(old); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	k, ok := r.GetKey("ak-old-1234567890abcdef")
	if !ok {
		t.Fatalf("GetKey 应 ok=true")
	}
	if k.Role != RoleUser {
		t.Errorf("旧 Key Role 空值应归一为 RoleUser, got %q", k.Role)
	}

	// 显式 role 字段保留。
	r2 := NewRing()
	if err := r2.Replace([]Key{{AK: "ak-n-1234567890abcdef", Role: RoleAdmin}}); err != nil {
		t.Fatalf("Replace(显式 role): %v", err)
	}
	if k, _ := r2.GetKey("ak-n-1234567890abcdef"); k.Role != RoleAdmin {
		t.Errorf("显式 Role 应保留, got %q", k.Role)
	}
}

// TestRing_AddRegistration 验证注册的原子 admin 判定 / 简单模式 / TOTP 模式（I2，DEC-A/DEC-B）。
func TestRing_AddRegistration(t *testing.T) {
	const ttl = 30 * 24 * time.Hour
	ak1 := "ak-reg-1111111111111111"
	ak2 := "ak-reg-2222222222222222"

	t.Run("首注册恒 admin 无论入参", func(t *testing.T) {
		clk := &mutableClock{}
		r := NewRing(clk.Now)
		granted, id, err := r.AddRegistration(ak1, "owner-a", must32BHex(t, 1), nil, RoleNode, ttl)
		if err != nil {
			t.Fatalf("AddRegistration: %v", err)
		}
		if !granted {
			t.Errorf("首注册应 granted=true")
		}
		k, ok := r.GetKey(ak1)
		if !ok {
			t.Fatalf("GetKey(ak1) 应 ok=true")
		}
		if k.Role != RoleAdmin {
			t.Errorf("首注册 Role 应为 admin（无论入参 node）, got %q", k.Role)
		}
		// 简单模式：id 前缀 skey-；SK 条目 32 字节；ExpiresAt = now+ttl（R4-I1，注入时钟）。
		if !strings.HasPrefix(id, SkeyIDPrefix) {
			t.Errorf("简单模式 id 应以 %q 开头, got %q", SkeyIDPrefix, id)
		}
		if len(k.Entries) != 1 {
			t.Fatalf("简单模式应有 1 条 SK 条目, got %d", len(k.Entries))
		}
		if len(k.Entries[0].SK) != 32 {
			t.Errorf("SK 条目应 32 字节, got %d", len(k.Entries[0].SK))
		}
		wantExp := fixedNow.Add(ttl)
		if !k.Entries[0].ExpiresAt.Equal(wantExp) {
			t.Errorf("SK 条目 ExpiresAt 应为 now+ttl=%v, got %v", wantExp, k.Entries[0].ExpiresAt)
		}
	})

	t.Run("非首注册降级 user", func(t *testing.T) {
		r := NewRing()
		if _, _, err := r.AddRegistration(ak1, "o1", must32BHex(t, 1), nil, RoleUser, ttl); err != nil {
			t.Fatalf("AddRegistration #1: %v", err)
		}
		// 非首注册传 RoleAdmin → 降级 user（不产生第二个 admin）。
		granted, _, err := r.AddRegistration(ak2, "o2", must32BHex(t, 2), nil, RoleAdmin, ttl)
		if err != nil {
			t.Fatalf("AddRegistration #2(RoleAdmin 入参): %v", err)
		}
		if granted {
			t.Errorf("非首注册传 RoleAdmin 应 granted=false")
		}
		if k, _ := r.GetKey(ak2); k.Role != RoleUser {
			t.Errorf("非首注册传 RoleAdmin 应降级 user, got %q", k.Role)
		}
		// 普通非首注册（默认 user 入参）→ user。
		ak3 := "ak-reg-3333333333333333"
		granted, _, err = r.AddRegistration(ak3, "o3", must32BHex(t, 3), nil, RoleUser, ttl)
		if err != nil {
			t.Fatalf("AddRegistration #3: %v", err)
		}
		if granted {
			t.Errorf("第二个非首注册应 granted=false")
		}
		if k, _ := r.GetKey(ak3); k.Role != RoleUser {
			t.Errorf("普通非首注册 Role 应为 user, got %q", k.Role)
		}
		// 断言整个 ring 只有一个 admin。
		admins := 0
		for _, kk := range r.Snapshot() {
			if kk.Role == RoleAdmin {
				admins++
			}
		}
		if admins != 1 {
			t.Errorf("全 ring admin 数应为 1, got %d", admins)
		}
	})

	t.Run("owner 空默认 AK", func(t *testing.T) {
		r := NewRing()
		if _, _, err := r.AddRegistration(ak1, "", must32BHex(t, 1), nil, RoleUser, ttl); err != nil {
			t.Fatalf("AddRegistration: %v", err)
		}
		k, _ := r.GetKey(ak1)
		if k.Owner != ak1 {
			t.Errorf("owner 空应默认 = AK, got %q", k.Owner)
		}
	})

	t.Run("TOTP 模式", func(t *testing.T) {
		clk := &mutableClock{}
		r := NewRing(clk.Now)
		secret := []byte("01234567890123456789012345678901")
		granted, id, err := r.AddRegistration(ak1, "o1", nil, secret, RoleUser, ttl)
		if err != nil {
			t.Fatalf("AddRegistration(TOTP): %v", err)
		}
		if !granted {
			t.Errorf("TOTP 首注册应 granted=true")
		}
		if id != "" {
			t.Errorf("TOTP 模式 id 应为空串, got %q", id)
		}
		k, _ := r.GetKey(ak1)
		if len(k.Entries) != 0 {
			t.Errorf("TOTP 模式不应有 SK 条目（ttl 忽略）, got %d", len(k.Entries))
		}
		if !bytes.Equal(k.TOTPSecret, secret) {
			t.Errorf("TOTP 模式应写入 TOTPSecret")
		}
	})

	t.Run("双 nil 报错", func(t *testing.T) {
		r := NewRing()
		if _, _, err := r.AddRegistration(ak1, "o1", nil, nil, RoleUser, ttl); err == nil {
			t.Errorf("双 nil（sk 与 totpSecret 均 nil）应返回 error")
		}
	})

	t.Run("简单模式深拷贝 SK", func(t *testing.T) {
		r := NewRing()
		sk := must32BHex(t, 0x77)
		if _, _, err := r.AddRegistration(ak1, "o1", sk, nil, RoleUser, ttl); err != nil {
			t.Fatalf("AddRegistration: %v", err)
		}
		sk[0] = 0xEE
		k, _ := r.GetKey(ak1)
		if k.Entries[0].SK[0] == 0xEE {
			t.Errorf("简单模式未深拷贝 SK（调用方改写污染 ring）")
		}
	})

	t.Run("TOTP 模式深拷贝 TOTPSecret", func(t *testing.T) {
		r := NewRing()
		secret := []byte("01234567890123456789012345678901")
		if _, _, err := r.AddRegistration(ak1, "o1", nil, secret, RoleUser, ttl); err != nil {
			t.Fatalf("AddRegistration: %v", err)
		}
		secret[0] = 0xff
		k, _ := r.GetKey(ak1)
		if k.TOTPSecret[0] == 0xff {
			t.Errorf("TOTP 模式未深拷贝 TOTPSecret（调用方改写污染 ring）")
		}
	})
}
