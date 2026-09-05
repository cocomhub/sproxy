// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"strings"
	"testing"
	"time"
	// P573 防循环：pkg/accesskey 主包不 import pkg/tunnel（tunnel 薄委托 accesskey，
	// 反向依赖会成环）；ParseMesh 与 tunnel 委托的等价性由 pkg/tunnel/tunnel_test.go
	// TestAccessKeyMesh 的同一语料保证，本文件不再直接引用 tunnel。
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

// constLegacyAKHex randHex(16) 的别名（test 用本地 16hex AK 段）。
const constLegacyAKHex = "1234567890abcdef"

// TestParseMesh 覆盖 mesh 段解析（标准 32hex 与 legacy 16hex 双兼容）。
// 语义与 pkg/tunnel.AccessKeyMesh（薄委托本实现）一致——同一语料两种入口都断言，
// 保证委托后无回归。
func TestParseMesh(t *testing.T) {
	tests := []struct {
		ak   string
		want string
	}{
		// 标准 32hex（16B）
		{"sk-prod-1234567890abcdef1234567890abcdef", "prod"},
		{"sk-prod-eu-1234567890abcdef1234567890abcdef", "prod-eu"}, // mesh 含连字符
		{"sk-meshA-3f8a1234abcd5678abcdef0123456789", "meshA"},
		{"sk-PROD-1234567890ABCDEF1234567890ABCDEF", "PROD"}, // 大写 hex
		{"sk-mesh-ABCDEF0123456789ABCDEF0123456789", "mesh"}, // 大写 hex 段
		{"sk-1234567890abcdef1234567890abcdef", ""},          // 无 mesh 段
		// legacy 16hex（8B，前向兼容：既有 16hex 凭据/网格仍能解析 mesh）
		{"sk-prod-1234567890abcdef", "prod"},
		{"sk-prod-eu-1234567890abcdef", "prod-eu"},
		{"sk-1234567890abcdef", ""},
		// 非法形态
		{"other", ""}, // 非白名单前缀
		{"ak-1234567890abcdef1234567890abcdef", ""},  // ak- 前缀不在白名单
		{"hub-1234567890abcdef1234567890abcdef", ""}, // 未来类型前缀未入白名单
		{"sk-", ""},                                       // 只有前缀
		{"sk-prod-1234567890abcde", ""},                   // hex 段不足（15）
		{"sk-prod-1234567890abcd", ""},                    // 12 hex 也不接受
		{"sk-prod-1234567890abcdeg", ""},                  // hex 段含非法字符
		{"sk-prod-1234567890abcdef1234567890abcdefg", ""}, // 33 hex 超长
		{"", ""},
	}
	for _, tt := range tests {
		if got := ParseMesh(tt.ak); got != tt.want {
			t.Errorf("ParseMesh(%q) = %q, want %q", tt.ak, got, tt.want)
		}
	}
}

// TestParseMesh_MultipleEntryPointsAccessKeyMesh 断言 tunnel 入口结果与本地一致
// （经 tunnel_test 全语料另证等价；此处仅补充 MeshFrom 别名）。
func TestParseMesh_MultipleEntryPointsAccessKeyMesh(t *testing.T) {
	if got := MeshFrom("sk-a-1234567890abcdef"); got != "a" {
		t.Errorf("MeshFrom legacy = %q, want a", got)
	}
	if got := MeshFrom("sk-b-1234567890abcdef1234567890abcdef"); got != "b" {
		t.Errorf("MeshFrom standard = %q, want b", got)
	}
}

// TestParseMesh_Legacy16HexMesh 专项断言：legacy 16hex（既有 8B 凭据）段本身含 hex
// 字母且用作"无 mesh"或"旧网格"时，mesh 解析必须正确（不破坏兼容）。
func TestParseMesh_Legacy16HexMesh(t *testing.T) {
	for _, mesh := range []string{"prod", "prod-eu", "meshA"} {
		ak := "sk-" + mesh + "-" + constLegacyAKHex
		if got := ParseMesh(ak); got != mesh {
			t.Errorf("legacy16hex ParseMesh(%q) = %q, want %q", ak, got, mesh)
		}
	}
	if got := ParseMesh("sk-" + constLegacyAKHex); got != "" {
		t.Errorf("ParseMesh(sk-<16hex>) = %q, want \"\"", got)
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
// `trust ak add` 的等价生成）：ak 前缀 / mesh 段 / 随机段 32hex(16B) / sk 64-hex / 两次不同。
func TestGeneratePair(t *testing.T) {
	ak, sk, err := GeneratePair(nil, "")
	if err != nil {
		t.Fatalf("GeneratePair: %v", err)
	}
	if !strings.HasPrefix(ak, "sk-") {
		t.Errorf("expected ak to start with sk-, got: %q", ak)
	}
	// 标准形态：无 mesh → len(ak) == len(AccessKeyPrefix)+AccessKeyHexLen*2（32 hex 随机段）。
	if len(ak) != len(AccessKeyPrefix)+AccessKeyHexLen*2 {
		t.Errorf("ak 随机段应为 %d hex（%d 字节）: got ak=%q (len=%d)",
			AccessKeyHexLen*2, AccessKeyHexLen, ak, len(ak))
	}
	// AK 随机段全 hex 且高熵（非全 0 / 非全 f）。
	randPart := strings.TrimPrefix(ak, AccessKeyPrefix)
	if !isValidHexLen(randPart, AccessKeyHexLen*2) {
		t.Errorf("ak 随机段应全为 %d 个 hex 字符, got %q", AccessKeyHexLen*2, randPart)
	}
	if isLowEntropy(randPart) {
		t.Errorf("ak 随机段不应为低熵重复形态, got %q", randPart)
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

// isValidHexLen 判断 s 是否恰为 n 个 hex 字符。
func isValidHexLen(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHexChar(s[i]) {
			return false
		}
	}
	return true
}

// isLowEntropy 粗略检测低熵随机段（全 0 / 全 f / 全同字符），用于高熵断言。
func isLowEntropy(s string) bool {
	if s == "" {
		return true
	}
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

func TestGeneratePair_MeshPrefix(t *testing.T) {
	ak, _, err := GeneratePair(nil, "meshA")
	if err != nil {
		t.Fatalf("GeneratePair(meshA): %v", err)
	}
	if !strings.HasPrefix(ak, "sk-meshA-") {
		t.Errorf("expected ak with sk-meshA- prefix, got: %q", ak)
	}
	if len(ak) != len("sk-meshA-")+AccessKeyHexLen*2 {
		t.Errorf("ak len = %d, want %d", len(ak), len("sk-meshA-")+AccessKeyHexLen*2)
	}
}

// TestGeneratePair_InjectReader GeneratePair 支持注入确定性 reader（cmd 测试可据此
// 断言生成内容，无需真实随机）。
func TestGeneratePair_InjectReader(t *testing.T) {
	// 16B ak 随机 + 32B sk 随机 = 48B；content 前 48B 即消耗读满。
	content := append(bytes.Repeat([]byte{0x11}, AccessKeyHexLen), bytes.Repeat([]byte{0x22}, 32)...)
	rd := bytes.NewReader(content)
	ak, sk, err := GeneratePair(rd, "")
	if err != nil {
		t.Fatalf("GeneratePair(inject): %v", err)
	}
	if !strings.HasPrefix(ak, "sk-") {
		t.Errorf("expected ak prefix, got: %q", ak)
	}
	if len(ak) != len(AccessKeyPrefix)+AccessKeyHexLen*2 {
		t.Errorf("ak len = %d, want %d", len(ak), len(AccessKeyPrefix)+AccessKeyHexLen*2)
	}
	// ak 随机段 = 16 个 0x11 → "1111...1111"（32 hex）。
	wantAK := AccessKeyPrefix + strings.Repeat("11", AccessKeyHexLen)
	if ak != wantAK {
		t.Errorf("ak = %q, want %q（确定性 reader 应精确消费）", ak, wantAK)
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

// TestIsValidAK 覆盖前缀白名单 / 随机段长度双兼容 / mesh 字符集校验。
func TestIsValidAK(t *testing.T) {
	tests := []struct {
		ak   string
		want bool
	}{
		{"sk-1234567890abcdef1234567890abcdef", true},          // 标准 32hex 无 mesh
		{"sk-prod-1234567890abcdef1234567890abcdef", true},     // 标准 32hex 带 mesh
		{"sk-prod-eu-1234567890abcdef1234567890abcdef", true},  // mesh 含连字符
		{"sk-mesh_a-1234567890abcdef1234567890abcdef", true},   // mesh 含下划线
		{"sk-1234567890abcdef", true},                          // legacy 16hex
		{"sk-prod-1234567890abcdef", true},                     // legacy 16hex 带 mesh
		{"sk-ABCDEF0123456789ABCDEF0123456789", true},          // 大写 hex
		{"ak-1234567890abcdef1234567890abcdef", false},         // 前缀不在白名单
		{"hub-1234567890abcdef1234567890abcdef", false},        // 未来类型前缀未入白名单
		{"totp-1234567890abcdef1234567890abcdef", false},       // 同上
		{"sk-1234567890abcdef1234567890abcdeg", false},         // hex 段含 g
		{"sk-1234567890abcdef1234567890abcde", false},          // 31 hex 长度不符
		{"sk-1234567890abcdef1", false},                        // 17 hex 长度不符
		{"sk-", false},                                         // 只有前缀
		{"sk", false},                                          // 无前缀连接符
		{"sk-prod-", false},                                    // 空 hex 段
		{"sk-pr od-1234567890abcdef1234567890abcdef", false},   // mesh 含空格
		{"sk-prod..x-1234567890abcdef1234567890abcdef", false}, // mesh 含非法字符（.）
		{"sk-prod+/x-1234567890abcdef1234567890abcdef", false}, // mesh 含非法字符
		{"other", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsValidAK(tt.ak); got != tt.want {
			t.Errorf("IsValidAK(%q) = %v, want %v", tt.ak, got, tt.want)
		}
	}
}

// TestAKConstantsContract 锁定前缀/长度常量（4B 类型前缀扩展点的单一事实源）；
// 若未来新增前缀（hub-/relay-/totp-/api-），只扩充 AllowedAKPrefixes 即可。
func TestAKConstantsContract(t *testing.T) {
	if AccessKeyPrefix != "sk-" {
		t.Errorf("AccessKeyPrefix = %q, want %q", AccessKeyPrefix, "sk-")
	}
	if len(AllowedAKPrefixes) != 1 || AllowedAKPrefixes[0] != "sk-" {
		t.Errorf("AllowedAKPrefixes = %v, want [sk-]", AllowedAKPrefixes)
	}
	// 生成字节数 = AccessKeyHexLen（16B）；解析双兼容常量 = 8B legacy。
	if AccessKeyHexLen != 16 || AccessKeyHexLegacy != 8 {
		t.Errorf("AccessKeyHexLen/AccessKeyHexLegacy = %d/%d, want 16/8", AccessKeyHexLen, AccessKeyHexLegacy)
	}
	// 随机段 hex 字符数：标准 = AccessKeyHexLen*2；legacy = AccessKeyHexLegacy*2。
	if AccessKeyHexLen*2 != 32 || AccessKeyHexLegacy*2 != 16 {
		t.Errorf("随机段标准/legacy = %d/%d hex, want 32/16", AccessKeyHexLen*2, AccessKeyHexLegacy*2)
	}
}

// TestStandardAKRandomSegmentLen 断言生成侧产出恒为 32hex（16B）随机段：
// 多次生成（含 mesh）随机段长度锁定为 AccessKeyHexLen*2 —— 生成侧不打 legacy 16hex。
func TestStandardAKRandomSegmentLen(t *testing.T) {
	for range 5 {
		ak, _, err := GeneratePair(nil, "")
		if err != nil {
			t.Fatalf("GeneratePair: %v", err)
		}
		if len(ak) != len(AccessKeyPrefix)+AccessKeyHexLen*2 {
			t.Errorf("标准生成 AK 随机段应为 32 hex, got %q (len=%d)", ak, len(ak))
		}
	}
	if len(AllowedAKPrefixes) != 1 || AllowedAKPrefixes[0] != "sk-" {
		t.Errorf("AllowedAKPrefixes 白名单应只含 sk-: %v", AllowedAKPrefixes)
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
