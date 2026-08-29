// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

// cleanupWebrtcGlobals 恢复 STUN/TURN 全局变量到默认状态，防止测试间及对其他
// 测试的污染（SetSTUNServers(nil) 按既有代码语义恢复默认 STUN 列表）。
func cleanupWebrtcGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetTURNServers(nil)
		SetTURNCredential("", "")
		SetSTUNServers(nil)
	})
}

// TestSetTURNServers_FiltersAndResets 验证 SetTURNServers 行为：
//   - 过滤空串与非 TURN/STUN scheme 的非法 URL（非 TURN 前缀直接忽略）
//   - 保留合法 turn:/turns: 与 stun: URL
//   - 传入 nil 恢复为空（清空之前设置的 TURN 服务器）
func TestSetTURNServers_FiltersAndResets(t *testing.T) {
	cleanupWebrtcGlobals(t)

	SetTURNServers([]string{"turn:relay.example.com:3478", "  ", "turns:relay-secure.example.com:5349", "notaurl", ""})
	got := append([]string(nil), turnServers...)
	if len(got) != 2 {
		t.Fatalf("过滤后应剩 2 个 TURN 服务器，实际 %d: %v", len(got), got)
	}
	if got[0] != "turn:relay.example.com:3478" {
		t.Errorf("got[0] = %q, want turn:relay.example.com:3478", got[0])
	}
	if got[1] != "turns:relay-secure.example.com:5349" {
		t.Errorf("got[1] = %q, want turns:relay-secure.example.com:5349", got[1])
	}

	SetTURNServers(nil)
	if len(turnServers) != 0 {
		t.Errorf("SetTURNServers(nil) 后 turnServers 应为空，实际 %d", len(turnServers))
	}
}

// TestSetTURNCredential_Stores 验证 SetTURNCredential 存储 user/pass。
func TestSetTURNCredential_Stores(t *testing.T) {
	cleanupWebrtcGlobals(t)

	SetTURNCredential("user1", "pass1")
	if turnUsername != "user1" {
		t.Errorf("turnUsername = %q, want user1", turnUsername)
	}
	if turnPassword != "pass1" {
		t.Errorf("turnPassword = %q, want pass1", turnPassword)
	}
}

// TestDefaultConfig_TURNEntryPresent 验证设置了 TURN 服务器与凭据后，
// defaultConfig() 产出包含 Username/Credential 的 TURN ICE server 条目。
func TestDefaultConfig_TURNEntryPresent(t *testing.T) {
	cleanupWebrtcGlobals(t)
	SetTURNServers([]string{"turn:relay.example.com:3478"})
	SetTURNCredential("user1", "pass1")

	cfg := defaultConfig()
	if len(cfg.ICEServers) == 0 {
		t.Fatal("defaultConfig() 应包含 ICEServers")
	}
	var turnEntry *webrtc.ICEServer
	for i := range cfg.ICEServers {
		entry := &cfg.ICEServers[i]
		for _, u := range entry.URLs {
			if len(u) >= len("turn:") && u[:len("turn:")] == "turn:" {
				turnEntry = entry
			}
		}
	}
	if turnEntry == nil {
		t.Fatal("defaultConfig() 缺少 TURN ICE server 条目")
	}
	if turnEntry.Username != "user1" {
		t.Errorf("TURN Username = %q, want user1", turnEntry.Username)
	}
	if turnEntry.Credential != "pass1" {
		t.Errorf("TURN Credential = %q, want pass1", turnEntry.Credential)
	}
	if turnEntry.CredentialType != webrtc.ICECredentialTypePassword {
		t.Errorf("TURN CredentialType = %v, want password", turnEntry.CredentialType)
	}
}

// TestDefaultConfig_NoTURNEntryWithoutCredential 验证未设置凭据时 defaultConfig()
// 不产生 TURN 条目（pion 4.2.18 对无凭据的 turn URL 报 ErrNoTurnCredentials，
// 因此无凭据时不能下发 turn ICEServer，否则 newPC 会失败）。
func TestDefaultConfig_NoTURNEntryWithoutCredential(t *testing.T) {
	cleanupWebrtcGlobals(t)

	// 有 TURN 服务器、无凭据 → 不得出现 turn ICEServer 条目
	SetTURNServers([]string{"turn:relay.example.com:3478"})
	cfg := defaultConfig()
	assertNoTurnEntry(t, cfg)

	// 有 TURN 服务器与用户但没有密码 → 同样不得出现
	SetTURNCredential("user1", "")
	cfg = defaultConfig()
	assertNoTurnEntry(t, cfg)

	// 无 TURN 服务器（STUN 默认恢复）→ 不得出现 turn 条目
	SetTURNCredential("user1", "pass1")
	SetTURNServers(nil)
	cfg = defaultConfig()
	assertNoTurnEntry(t, cfg)
}

// assertNoTurnEntry 断言配置中没有任何以 turn: 开头的 URLs 的 ICEServer 条目。
func assertNoTurnEntry(t *testing.T, cfg webrtc.Configuration) {
	t.Helper()
	for _, entry := range cfg.ICEServers {
		for _, u := range entry.URLs {
			if len(u) >= len("turn:") && u[:len("turn:")] == "turn:" {
				t.Fatalf("defaultConfig() 意外包含 TURN 条目: %+v", cfg)
			}
		}
	}
}

// TestDefaultConfig_HostOnlySuppresses 验证 useHostOnly 开启时 defaultConfig() 返回空。
func TestDefaultConfig_HostOnlySuppresses(t *testing.T) {
	cleanupWebrtcGlobals(t)
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	SetTURNServers([]string{"turn:relay.example.com:3478"})
	SetTURNCredential("user1", "pass1")

	if len(defaultConfig().ICEServers) != 0 {
		t.Fatalf("useHostOnly 时 defaultConfig() 应返回空配置: %+v", defaultConfig())
	}
}

// TestDefaultConfig_STUNEntryPresent 验证既有 STUN 行为未被 TURN 改动破坏：
// 默认 STUN 列表 + 凭据时仍有 STUN 条目与 TURN 条目并存。
func TestDefaultConfig_STUNEntryPresent(t *testing.T) {
	cleanupWebrtcGlobals(t)
	SetTURNServers([]string{"turn:relay.example.com:3478"})
	SetTURNCredential("user1", "pass1")

	cfg := defaultConfig()
	var stunEntry bool
	for _, entry := range cfg.ICEServers {
		for _, u := range entry.URLs {
			if len(u) >= len("stun:") && u[:len("stun:")] == "stun:" {
				stunEntry = true
			}
		}
	}
	if !stunEntry {
		t.Fatalf("defaultConfig() 缺少 STUN 条目（既有行为被破坏）: %+v", cfg)
	}
}
