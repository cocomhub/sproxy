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

// TestGeneratePair 覆盖 GeneratePair 生成逻辑（access-key 命令删除后内联进
// `trust ak add` 的等价生成）：ak 前缀 / mesh 段 / sk 64-hex / 两次不同。
func TestGeneratePair(t *testing.T) {
	ak, sk, err := GeneratePair(nil, "")
	if err != nil {
		t.Fatalf("GeneratePair: %v", err)
	}
	if !strings.HasPrefix(ak, "sk-") {
		t.Errorf("expected ak to start with sk-, got: %q", ak)
	}
	if len(sk) != 64 {
		t.Errorf("expected sk to be 64 hex chars, got %d", len(sk))
	}
	for _, c := range sk {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Errorf("sk 含非 hex 字符 %q", sk)
			break
		}
	}
	ak2, sk2, err := GeneratePair(nil, "")
	if err != nil {
		t.Fatalf("GeneratePair(second): %v", err)
	}
	if ak == ak2 || sk == sk2 {
		t.Error("expected two generated pairs to differ")
	}
}

func TestGeneratePair_MeshPrefix(t *testing.T) {
	ak, _, err := GeneratePair(nil, "meshA")
	if err != nil {
		t.Fatalf("GeneratePair(meshA): %v", err)
	}
	if !strings.HasPrefix(ak, "sk-meshA-") {
		t.Errorf("expected ak with sk-meshA- prefix, got: %q", ak)
	}
}

// TestGeneratePair_InjectReader GeneratePair 支持注入确定性 reader（cmd 测试可据此
// 断言生成内容，无需真实随机）。
func TestGeneratePair_InjectReader(t *testing.T) {
	// 8B ak 随机 + 32B sk 随机 = 40B；content 前 40B 即消耗读满。
	content := append(bytes.Repeat([]byte{0x11}, 8), bytes.Repeat([]byte{0x22}, 32)...)
	rd := bytes.NewReader(content)
	ak, sk, err := GeneratePair(rd, "")
	if err != nil {
		t.Fatalf("GeneratePair(inject): %v", err)
	}
	if !strings.HasPrefix(ak, "sk-") {
		t.Errorf("expected ak prefix, got: %q", ak)
	}
	if len(ak) != len("sk-")+AccessKeyHexLen {
		t.Errorf("ak len = %d, want %d", len(ak), len("sk-")+AccessKeyHexLen)
	}
	wantSK := strings.Repeat("22", 32)
	if sk != wantSK {
		t.Errorf("sk = %q, want %q（确定性 reader 应精确消费）", sk, wantSK)
	}
	short := bytes.NewReader(make([]byte, 4)) // 不足以读满 ak → 必须报错
	if _, _, err := GeneratePair(short, ""); err == nil {
		t.Error("expected error when reader is too short")
	}
}

// TestWrapContextCredentials 断言凭据 wrap context 常量值（M5 收归后唯一事实源，
// server 与 client 分别以别名引用；此处锁定该字面量防止无意改动破坏双端派生）。
func TestWrapContextCredentials(t *testing.T) {
	if WrapContextCredentials != "sproxy-credentials/v1" {
		t.Errorf("WrapContextCredentials = %q, want %q", WrapContextCredentials, "sproxy-credentials/v1")
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
