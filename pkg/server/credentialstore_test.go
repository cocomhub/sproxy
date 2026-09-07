// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// seedTestRing 构造一个含单 AK（ak/sk）的 Ring 快照（plain 条目）。
func seedTestRing(t *testing.T, ak, skHex string, expire bool) []accesskey.Key {
	t.Helper()
	sk, err := hex.DecodeString(skHex)
	if err != nil || len(sk) != 32 {
		t.Fatalf("skHex 非法: %q", skHex)
	}
	ring := accesskey.NewRing()
	if err := ring.UpsertAK(ak, "test"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	opts := []accesskey.EntryOption{accesskey.WithMeta(accesskey.Meta{Type: "initial"})}
	if expire {
		opts = append(opts, accesskey.WithExpiresAt(ring.CoreEntry(ak).CreatedAt.Add(-1)))
	}
	if _, err := ring.AddKey(ak, sk, opts...); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	return ring.Snapshot()
}

// TestCredentialStore_SaveLoadRoundtrip 验证 Save(ring 快照) → Load 等价还原
// （AK/条目/SK 字节一致）。
func TestCredentialStore_SaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	st := NewCredentialStore(filepath.Join(dir, "tenant-a", "meta"))

	orig := seedTestRing(t, "ak-test-aabbcc", testAccessSecret, false)
	if err := st.Save(orig); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(orig) {
		t.Fatalf("len = %d, want %d", len(got), len(orig))
	}
	if got[0].AK != orig[0].AK {
		t.Errorf("AK = %q, want %q", got[0].AK, orig[0].AK)
	}
	if len(got[0].Entries) != len(orig[0].Entries) {
		t.Fatalf("entries = %d, want %d", len(got[0].Entries), len(orig[0].Entries))
	}
	for i := range orig[0].Entries {
		want := orig[0].Entries[i]
		have := got[0].Entries[i]
		if have.ID != want.ID || have.Kind != want.Kind || have.Meta.Type != want.Meta.Type {
			t.Errorf("entry %d 元数据不一致: %+v vs %+v", i, have, want)
		}
		if string(have.SK) != string(want.SK) {
			t.Errorf("entry %d SK 字节不一致", i)
		}
	}
}

// TestCredentialStore_SaveLoadRoundtrip_AccountRoleAndTOTP 固化账号级字段的 JSON 落盘契约：
// Save→Load 往返后 Key.Role 与 Key.TOTPSecret 保留（json tag "role"/"totp_secret"）。
// 覆盖 R3-M4 兼容链路的另一端——新字段落盘后重启读回不丢。
func TestCredentialStore_SaveLoadRoundtrip_AccountRoleAndTOTP(t *testing.T) {
	dir := t.TempDir()
	st := NewCredentialStore(filepath.Join(dir, "tenant", "meta"))

	// TOTP 模式注册：首注册授 admin + 写 TOTPSecret（ttl 在 TOTP 模式被忽略，传 0）。
	ring := accesskey.NewRing()
	if _, _, err := ring.AddRegistration("ak-acct-1234567890abcdef", "owner-x", nil,
		[]byte("01234567890123456789012345678901"), accesskey.RoleUser, 0); err != nil {
		t.Fatalf("AddRegistration(TOTP): %v", err)
	}
	orig := ring.Snapshot()
	if err := st.Save(orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Role != accesskey.RoleAdmin {
		t.Errorf("Key.Role 未保留, got %q, want %q", got[0].Role, accesskey.RoleAdmin)
	}
	if string(got[0].TOTPSecret) != "01234567890123456789012345678901" {
		t.Errorf("Key.TOTPSecret 未保留, got %q", got[0].TOTPSecret)
	}
	// TOTP 模式注册无 SK 条目也保留（无 SK 条目侧）。
	if len(got[0].Entries) != 0 {
		t.Errorf("TOTP 模式注册不应有 SK 条目, got %d", len(got[0].Entries))
	}
}

// TestCredentialStore_LoadMissing 验证文件不存在时 Load 返回空（非错）。
func TestCredentialStore_LoadMissing(t *testing.T) {
	st := NewCredentialStore(filepath.Join(t.TempDir(), "tenant", "meta"))
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load 缺失文件应返回 nil,nil: %v", err)
	}
	if got != nil {
		t.Fatalf("got = %v, want nil", got)
	}
}

// TestCredentialStore_LoadCorrupt 验证文件损坏（非法 JSON）返回错误（fail-closed）。
func TestCredentialStore_LoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "tenant", "meta")
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(meta, "credentials.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := NewCredentialStore(meta)
	if _, err := st.Load(); err == nil {
		t.Fatal("损坏文件 Load 应返回 error")
	}
}

// TestCredentialStore_SaveNoTmpLeftover 验证 Save 后无 .tmp 残留。
func TestCredentialStore_SaveNoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	st := NewCredentialStore(filepath.Join(dir, "tenant", "meta"))
	if err := st.Save(seedTestRing(t, "ak-aabbcc", testAccessSecret, false)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "tenant", "meta"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("Save 后不应残留 .tmp: %s", e.Name())
		}
	}
}

// TestCredentialStore_ConcurrentSave 验证并发 Save 不损坏（-race）。
func TestCredentialStore_ConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	st := NewCredentialStore(filepath.Join(dir, "tenant", "meta"))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := st.Save(seedTestRing(t, "ak-aabbcc", testAccessSecret, false)); err != nil {
				t.Errorf("并发 Save: %v", err)
			}
		})
	}
	wg.Wait()
	// 最终文件必须可解析且等价（任一次 Save 的产物）。
	got, err := st.Load()
	if err != nil {
		t.Fatalf("并发 Save 后 Load: %v", err)
	}
	if len(got) != 1 || got[0].AK != "ak-aabbcc" {
		t.Fatalf("并发 Save 后文件损坏: %+v", got)
	}
}

// TestGenerateBootstrapCredential_Format 验证 register 端点 AK/SK 生成格式契约：
// 经 RegisterRoutes + 回环首注册产出的 AK 随机段 32hex(16B)、SK 64 hex——
// 委托 pkg/accesskey.GeneratePair（""），与 GeneratePair 同源同长（U3 移除首启
// anonymous 后，该格式断言改挂在公开注册端点的实际产物上，见 register_handler_test
// TestRegister_SimpleMode_Success。本测试保留对 accesskey.GeneratePair 的直接契约）。
func TestGenerateBootstrapCredential_Format(t *testing.T) {
	ak, sk, err := accesskey.GeneratePair(nil, "")
	if err != nil {
		t.Fatalf("GeneratePair: %v", err)
	}
	if !strings.HasPrefix(ak, accesskey.AccessKeyPrefix) {
		t.Errorf("AK 应以 %q 开头: %q", accesskey.AccessKeyPrefix, ak)
	}
	// AK 随机段恒 32 hex（16 字节）——服务端 register 生成标准形态。
	if len(ak) != len(accesskey.AccessKeyPrefix)+accesskey.AccessKeyHexLen*2 {
		t.Errorf("AK 随机段应为 %d hex(%dB): got %q (len=%d)",
			accesskey.AccessKeyHexLen*2, accesskey.AccessKeyHexLen, ak, len(ak))
	}
	if len(sk) != 64 {
		t.Errorf("SK 长度 = %d, want 64", len(sk))
	}
	if _, derr := hex.DecodeString(sk); derr != nil {
		t.Errorf("SK 非 hex: %v", derr)
	}
	// 熵等价断言：解析/校验通过官方入口。
	if !accesskey.IsValidAK(ak) {
		t.Errorf("产物应通过 IsValidAK: %q", ak)
	}
	if got := accesskey.ParseMesh(ak); got != "" {
		t.Errorf("无 mesh，ParseMesh = %q, want \"\"", got)
	}
	// 两次生成不同（随机性）。
	ak2, sk2, err := accesskey.GeneratePair(nil, "")
	if err != nil {
		t.Fatalf("GeneratePair(second): %v", err)
	}
	if ak == ak2 || sk == sk2 {
		t.Errorf("两次生成应不同")
	}
}

// TestCredentialStore_FileLayout 验证路径为 <metaDir>/credentials.json。
func TestCredentialStore_FileLayout(t *testing.T) {
	meta := filepath.Join(t.TempDir(), "tenant", "meta")
	st := NewCredentialStore(meta)
	if st.path != filepath.Join(meta, "credentials.json") {
		t.Fatalf("path = %q", st.path)
	}
	if err := st.Save(seedTestRing(t, "ak-aabbcc", testAccessSecret, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(meta, "credentials.json")); err != nil {
		t.Fatalf("credentials.json: %v", err)
	}
	// 磁盘 JSON 可反序列化为 credentialsFile（结构稳定契约）。
	data, err := os.ReadFile(filepath.Join(meta, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f credentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("磁盘 JSON 反序列化: %v", err)
	}
	if f.Version != 1 || len(f.Keys) != 1 {
		t.Fatalf("磁盘格式异常: %+v", f)
	}
}

// valueStorer 是仅用于 TestNormalizeStorer 的非指针值类型 CredentialStorer 实现
// （验证 normalizeStorer 对值类型入参原样返回，不误归一为 nil）。
type valueStorer struct{ tag string }

func (valueStorer) Load() ([]accesskey.Key, error) { return nil, nil }
func (valueStorer) Save([]accesskey.Key) error     { return nil }

// TestNormalizeStorer 验证 normalizeStorer 的 nil 归一语义（审查 M1）：
//   - 字面 nil → nil；
//   - typed-nil `(*CredentialStore)(nil)` 装箱进接口 → nil（消除 persistCredentials
//     对 nil 接收者调 Save 的 panic 根因）；
//   - 真指针 → 原样同一值；
//   - 非指针值类型实现（valueStorer 满足 CredentialStorer）→ 原样同一接口值。
func TestNormalizeStorer(t *testing.T) {
	real := NewCredentialStore(filepath.Join(t.TempDir(), "tenant", "meta"))

	cases := []struct {
		name    string
		in      accesskey.CredentialStorer
		wantNil bool
	}{
		{name: "字面nil", in: nil, wantNil: true},
		{name: "typed-nil指针", in: (*CredentialStore)(nil), wantNil: true},
		{name: "真指针非nil", in: real},
		{name: "非指针值类型", in: valueStorer{tag: "v"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeStorer(tc.in)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("应归一为 nil, got %#v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("非 nil 输入被误归一为 nil")
			}
			// 同一性：归一结果与入参指向/等效同一实例。
			switch v := tc.in.(type) {
			case *CredentialStore:
				if got.(*CredentialStore) != v {
					t.Fatalf("真指针应原样返回，got 另一个实例")
				}
			case valueStorer:
				stored, ok := got.(valueStorer)
				if !ok || stored != v {
					t.Fatalf("值类型应原样返回: got %#v, want %#v", got, v)
				}
			default:
				t.Fatalf("未预期的输入类型单向断言: %T", tc.in)
			}
		})
	}
}
