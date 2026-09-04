// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestKindStatus 枚举值契约（规格 5 固定值）。
func TestKindStatus(t *testing.T) {
	if string(KindPlain) != "plain" || string(KindSecretWrap) != "secret_wrap" || string(KindTOTPWrap) != "totp_wrap" {
		t.Fatalf("Kind 枚举值不符")
	}
	if string(StatusActive) != "active" || string(StatusExpired) != "expired" || string(StatusDisabled) != "disabled" {
		t.Fatalf("Status 枚举值不符")
	}
}

// TestParseMesh 覆盖 mesh 段解析，语义与 pkg/tunnel.AccessKeyMesh 一致。
func TestParseMesh(t *testing.T) {
	tests := []struct {
		ak   string
		want string
	}{
		{"sk-prod-1234567890abcdef", "prod"},
		{"sk-prod-eu-1234567890abcdef", "prod-eu"}, // mesh 含连字符
		{"sk-meshA-3f8a1234abcd5678", "meshA"},
		{"sk-PROD-1234567890ABCDEF", "PROD"}, // 大写 hex（与 tunnel hexChars 一致）
		{"sk-mesh-ABCDEF0123456789", "mesh"}, // 大写 hex 段
		{"sk-1234567890abcdef", ""},          // 无 mesh 段
		{"other", ""},                        // 非 sk- 前缀
		{"sk-", ""},                          // 只有前缀
		{"sk-prod-1234567890abcde", ""},      // hex 段不足 16 位
		{"sk-prod-1234567890abcdeg", ""},     // hex 段含非法字符
		{"", ""},
	}
	for _, tt := range tests {
		if got := ParseMesh(tt.ak); got != tt.want {
			t.Errorf("ParseMesh(%q) = %q, want %q", tt.ak, got, tt.want)
		}
	}
}

// TestNewEntryID_FormatAndUnique newEntryID 格式为 sk-<12hex> 且互不重复。
func TestNewEntryID_FormatAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := newEntryID()
		if err != nil {
			t.Fatalf("newEntryID: %v", err)
		}
		if !strings.HasPrefix(id, "sk-") {
			t.Fatalf("newEntryID 应以 sk- 开头, got %q", id)
		}
		if len(id) != len("sk-")+EntryIDLen {
			t.Fatalf("newEntryID 长度应为 sk-<12hex>, got %q (%d)", id, len(id))
		}
		for _, c := range id[len("sk-"):] {
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				t.Fatalf("newEntryID 的 hex 段含非法字符 %q", id)
			}
		}
		if seen[id] {
			t.Fatalf("newEntryID 出现重复 %q", id)
		}
		seen[id] = true
	}
}

// TestGetEntry_NotFound 不存在的条目返回 ErrNotFound。
func TestGetEntry_NotFound(t *testing.T) {
	r := NewRing()
	if _, _, err := r.GetEntry("sk-x", "sk-000000000000"); err != ErrNotFound {
		t.Fatalf("不存在条目应 ErrNotFound, got %v", err)
	}
}

// TestExpireKey_StatusExpiredUntil 设一个未来时间会设置 ExpiresAt，届时条目过期。
// 用可变时钟验证 alive 判定（含 ExpiresAt 已到）。
func TestExpireKey_StatusExpiredUntil(t *testing.T) {
	clk := &mutableClock{}
	r := NewRing(clk.Now)
	ak := "sk-t1-1234567890abcdef"
	if err := r.UpsertAK(ak, "o"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	id, err := r.AddKey(ak, must32BHex(t, 9))
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	until := fixedNow.Add(time.Hour)
	if err := r.ExpireKey(ak, id, until); err != nil {
		t.Fatalf("ExpireKey: %v", err)
	}
	// 未到时间仍 alive
	if _, alive, err := r.GetEntry(ak, id); err != nil || !alive {
		t.Fatalf("未到 ExpiresAt 应 alive")
	}
	// 前进 2h → 过期
	clk.Advance(2 * time.Hour)
	if _, _, err := r.GetEntry(ak, id); err != ErrExpired {
		t.Fatalf("到达 ExpiresAt 后应 ErrExpired, got %v", err)
	}
	if ce := r.CoreEntry(ak); ce != nil {
		t.Fatalf("CoreEntry 应无 alive 条目")
	}
}

// TestSKEntry_ZeroValues SKEntry 零值 ExpiresAt 表示永久有效（aliveLocked 语义）。
func TestSKEntry_ZeroValues(t *testing.T) {
	now := fixedNow
	e := SKEntry{Status: StatusActive, ExpiresAt: time.Time{}, CreatedAt: now}
	if !aliveLocked(e, now.Add(time.Hour*24*365)) {
		t.Fatalf("ExpiresAt 零值条目应永久 alive")
	}
}

// TestWrappedSecret_StructFields WrappedSecret/Meta 字段契约（wrap JSON tag 由 wrap_test 覆盖）。
func TestWrappedSecret_StructFields(t *testing.T) {
	ws := WrappedSecret{
		Kind:      KindSecretWrap,
		WrapKeyID: "sk-w-1234567890abcdef",
		Nonce:     []byte{0x01, 0x02},
		Cipher:    []byte{0x03, 0x04},
	}
	if ws.Kind != KindSecretWrap || len(ws.Nonce) != 2 || len(ws.Cipher) != 2 || ws.WrapKeyID == "" {
		t.Fatalf("WrappedSecret 字段应可读写")
	}
	if !bytes.Equal(ws.Nonce, []byte{0x01, 0x02}) {
		t.Fatalf("Nonce 应保留")
	}
	m := Meta{Type: "renew", IP: "127.0.0.1"}
	if m.Type != "renew" || m.IP != "127.0.0.1" {
		t.Fatalf("Meta 字段不符")
	}
}
