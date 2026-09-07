// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sampleEncryptingKeys 构造一个含账号级字段（Role/TOTPSecret）+ 多条 SK 条目（含
// secret_wrap 形态）的凭据快照，供加密静态存储往返断言使用。
func sampleEncryptingKeys() []Key {
	created := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	expires := created.Add(24 * time.Hour)
	return []Key{
		{
			AK:         "ak-admin-0123456789abcdef0123456789abcdef",
			Owner:      "owner-admin",
			Role:       RoleAdmin,
			TOTPSecret: []byte("01234567890123456789012345678901"),
		},
		{
			AK:    "ak-user-0123456789abcdef0123456789abcdef",
			Owner: "owner-user",
			Role:  RoleUser,
			Entries: []SKEntry{
				{
					ID:        "skey-abcdef012345",
					SK:        []byte("01234567890123456789012345678901"),
					Kind:      KindPlain,
					Status:    StatusActive,
					CreatedAt: created,
					ExpiresAt: expires,
					Meta:      Meta{Type: "initial", IP: "127.0.0.1"},
				},
				{
					ID:        "skey-abcdef012346",
					SK:        []byte("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"),
					Kind:      KindSecretWrap,
					WrapKeyID: "ak-user-0123456789abcdef0123456789abcdef",
					Status:    StatusActive,
					CreatedAt: created.Add(time.Hour),
					Meta:      Meta{Type: "renew"},
				},
			},
		},
	}
}

// assertKeysDeepEqual 深比较两个 Key 快照（AK/Owner/Role/TOTPSecret/SK 字节/条目元数据/
// 时间逐字段），仿 TestCredentialStore_SaveLoadRoundtrip_AccountRoleAndTOTP 断言风格。
func assertKeysDeepEqual(t *testing.T, got, want []Key) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.AK != w.AK || g.Owner != w.Owner || g.Role != w.Role {
			t.Errorf("key %d 账号字段不一致: %+v vs %+v", i, g, w)
		}
		if string(g.TOTPSecret) != string(w.TOTPSecret) {
			t.Errorf("key %d TOTPSecret 不一致: %q vs %q", i, g.TOTPSecret, w.TOTPSecret)
		}
		if len(g.Entries) != len(w.Entries) {
			t.Fatalf("key %d entries = %d, want %d", i, len(g.Entries), len(w.Entries))
		}
		for j := range w.Entries {
			ge, we := g.Entries[j], w.Entries[j]
			if ge.ID != we.ID || ge.Kind != we.Kind || ge.WrapKeyID != we.WrapKeyID ||
				ge.Status != we.Status || ge.Meta.Type != we.Meta.Type || ge.Meta.IP != we.Meta.IP {
				t.Errorf("key %d entry %d 元数据不一致: %+v vs %+v", i, j, ge, we)
			}
			if string(ge.SK) != string(we.SK) {
				t.Errorf("key %d entry %d SK 字节不一致", i, j)
			}
			if !ge.CreatedAt.Equal(we.CreatedAt) || !ge.ExpiresAt.Equal(we.ExpiresAt) {
				t.Errorf("key %d entry %d 时间不一致: %+v vs %+v", i, j, ge.CreatedAt, we.CreatedAt)
			}
		}
	}
}

// TestEncryptingStorer_SaveLoadRoundtrip_Encrypted 验证加密静态存储的往返：
// Save（含 Role/TOTPSecret/多条 SK）→ 磁盘为密文（不含 "keys" 明文 JSON 字样且字节
// ≠ 明文）→ Load 还原出与 Save 前深等价的 Key 快照。
func TestEncryptingStorer_SaveLoadRoundtrip_Encrypted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant", "meta", "credentials.json")
	key := bytes.Repeat([]byte{0x42}, 32)
	st := NewEncryptingStorer(path, AESGCMStorer{Key: key})

	orig := sampleEncryptingKeys()
	if err := st.Save(orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// 加密态磁盘格式：密文字节，无明文 JSON 壳——断言不含 "keys" 字段字样。
	if bytes.Contains(disk, []byte(`"keys"`)) {
		t.Fatalf("加密态磁盘不应出现明文 JSON 的 \"keys\" 字样")
	}
	// 与明文 JSON 字节必须不同。
	plain := encryptedCredentialsFile{Version: 1, Keys: orig}
	if plainJSON, jerr := json.MarshalIndent(plain, "", "  "); jerr == nil && bytes.Equal(disk, plainJSON) {
		t.Fatalf("加密态磁盘字节不应等于明文 JSON")
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertKeysDeepEqual(t, got, orig)
}

// TestEncryptingStorer_LoadTamper 验证密文篡改一个字节后 Load 报错（GCM 认证失败，
// fail-closed，不静默重建）。
func TestEncryptingStorer_LoadTamper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant", "meta", "credentials.json")
	st := NewEncryptingStorer(path, AESGCMStorer{Key: bytes.Repeat([]byte{0x11}, 32)})
	if err := st.Save(sampleEncryptingKeys()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	disk[len(disk)/2] ^= 0x01 // 翻转中间一个密文字节
	if werr := os.WriteFile(path, disk, 0o600); werr != nil {
		t.Fatal(werr)
	}
	_, err = st.Load()
	if err == nil {
		t.Fatal("篡改密文后 Load 应返回 error（GCM auth 失败）")
	}
	// M-1：篡改密文（首字节随机，非明文）必须走「密文被篡改 / key 错」分支，不得被
	// looksLikePlaintextJSON 误判为明文未迁移（「未迁移」是明文嗅探分支的唯一提示 token）。
	if strings.Contains(err.Error(), "未迁移") {
		t.Fatalf("篡改密文不应提示明文未迁移: %v", err)
	}
	if !strings.Contains(err.Error(), "密文被篡改") {
		t.Fatalf("篡改密文应提示密文被篡改: %v", err)
	}
}

// TestEncryptingStorer_LoadWrongMasterKey 验证用不同 master key 解密已有密文报错
// （密钥错 → GCM 认证失败，fail-closed）。
func TestEncryptingStorer_LoadWrongMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant", "meta", "credentials.json")
	stA := NewEncryptingStorer(path, AESGCMStorer{Key: bytes.Repeat([]byte{0x01}, 32)})
	if err := stA.Save(sampleEncryptingKeys()); err != nil {
		t.Fatalf("Save(A): %v", err)
	}
	stB := NewEncryptingStorer(path, AESGCMStorer{Key: bytes.Repeat([]byte{0x02}, 32)})
	_, err := stB.Load()
	if err == nil {
		t.Fatal("用错误 master key Load 应返回 error")
	}
	// M-1：key 错时磁盘为真密文（首字节随机），须走「密文被篡改 / key 错」分支，
	// 不得被 looksLikePlaintextJSON 误判为明文未迁移（「未迁移」是明文嗅探分支的唯一
	// 提示 token）。
	if strings.Contains(err.Error(), "未迁移") {
		t.Fatalf("key 错不应提示明文未迁移: %v", err)
	}
	if !strings.Contains(err.Error(), "master key 不匹配") {
		t.Fatalf("key 错应提示 master key 不匹配: %v", err)
	}
}

// TestEncryptingStorer_PlainModeRoundtrip 验证 secure=nil 明文模式的往返 = 现状
// （磁盘为明文 JSON，Load 原样还原）。
func TestEncryptingStorer_PlainModeRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant", "meta", "credentials.json")
	st := NewEncryptingStorer(path, nil)

	orig := sampleEncryptingKeys()
	if err := st.Save(orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(disk, []byte(`"keys"`)) {
		t.Fatalf("明文模式磁盘应含明文 JSON 的 \"keys\" 字样")
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertKeysDeepEqual(t, got, orig)
}

// TestEncryptingStorer_LoadPlaintextFailsClosed 验证开启加密后 Load 到历史明文文件
// 报错（fail-closed 不静默改写/重建，提示需先迁移）。
func TestEncryptingStorer_LoadPlaintextFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant", "meta", "credentials.json")
	// 先用明文模式落盘（等价历史明文凭据文件）。
	plainSt := NewEncryptingStorer(path, nil)
	if err := plainSt.Save(sampleEncryptingKeys()); err != nil {
		t.Fatalf("Save(plain): %v", err)
	}
	// 换加密 storer Load → 明文文件被当作密文解密必失败。
	encSt := NewEncryptingStorer(path, AESGCMStorer{Key: bytes.Repeat([]byte{0x21}, 32)})
	_, err := encSt.Load()
	if err == nil {
		t.Fatal("加密 storer Load 历史明文文件应返回 error（fail-closed）")
	}
	// M-1：历史明文 JSON（'{' 开头 + 含 "keys"）必须走 looksLikePlaintextJSON 嗅探分支，
	// 给出「明文 JSON 未迁移」定向提示（「未迁移」是明文嗅探分支的唯一提示 token）。
	if !strings.Contains(err.Error(), "未迁移") {
		t.Fatalf("明文文件 Load 应提示明文未迁移: %v", err)
	}
}

// TestEncryptingStorer_LoadMissing 验证文件不存在时 Load 返回 (nil, nil)（首次启动）。
func TestEncryptingStorer_LoadMissing(t *testing.T) {
	st := NewEncryptingStorer(filepath.Join(t.TempDir(), "tenant", "meta", "credentials.json"), AESGCMStorer{Key: bytes.Repeat([]byte{0x31}, 32)})
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load 缺失文件应返回 nil,nil: %v", err)
	}
	if got != nil {
		t.Fatalf("got = %v, want nil", got)
	}
}

// TestEncryptWithKey_EnvelopeRoundtrip 验证字节级信封封装往返：EncryptWithKey 产物为
// nonce(12B) || ciphertext（GCM tag 含尾），DecryptWithKey 还原明文。
func TestEncryptWithKey_EnvelopeRoundtrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	payload := bytes.Repeat([]byte("credentials-json-payload-中文"), 10)
	sealed, err := EncryptWithKey(key, payload)
	if err != nil {
		t.Fatalf("EncryptWithKey: %v", err)
	}
	if len(sealed) != 12+len(payload)+gcmTagSize {
		t.Fatalf("sealed len = %d, want nonce12 + payload %d + tag16", len(sealed), len(payload))
	}
	got, err := DecryptWithKey(key, sealed)
	if err != nil {
		t.Fatalf("DecryptWithKey: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("解密结果与明文不一致")
	}
}

// TestEncryptWithKey_WrongKeyAndTamper 验证密钥错/密文篡改/坏格式均报错（fail-closed）。
func TestEncryptWithKey_WrongKeyAndTamper(t *testing.T) {
	keyA := bytes.Repeat([]byte{0x0a}, 32)
	keyB := bytes.Repeat([]byte{0x0b}, 32)
	payload := []byte("secrets")
	sealed, err := EncryptWithKey(keyA, payload)
	if err != nil {
		t.Fatalf("EncryptWithKey: %v", err)
	}
	if _, err := DecryptWithKey(keyB, sealed); err == nil {
		t.Fatal("错误 key 解密应报错")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := DecryptWithKey(keyA, tampered); err == nil {
		t.Fatal("篡改密文应报错")
	}
	if _, err := DecryptWithKey(keyA, []byte("too-short")); err == nil {
		t.Fatal("坏格式（短于 nonce）应报错")
	}
	// M-1：非 32B master key 返回专用哨兵 ErrInvalidMasterKey（而非 SK 语义的
	// ErrInvalidSecret，避免文案误导）。
	if _, err := EncryptWithKey([]byte("bad-key"), payload); !errors.Is(err, ErrInvalidMasterKey) {
		t.Fatalf("非 32B key 加密应返回 ErrInvalidMasterKey, got %v", err)
	}
	if _, err := DecryptWithKey([]byte("bad-key"), sealed); !errors.Is(err, ErrInvalidMasterKey) {
		t.Fatalf("非 32B key 解密应返回 ErrInvalidMasterKey, got %v", err)
	}
}

// TestDeriveMasterKey_Deterministic 验证 DeriveMasterKey 的派生确定性：
// 同 passphrase+salt 两次一致；salt 不同派生不同（HKDF 域分离）。
func TestDeriveMasterKey_Deterministic(t *testing.T) {
	pass := []byte("correct horse battery staple")
	salt := bytes.Repeat([]byte{0xaa}, 16)
	k1, err := DeriveMasterKey(pass, salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey: %v", err)
	}
	k2, err := DeriveMasterKey(pass, salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey(second): %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("derived key len = %d, want 32", len(k1))
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("同 passphrase+salt 派生应一致")
	}
	// salt 不同 → 派生不同（防跨用途/跨盐复用）。
	k3, err := DeriveMasterKey(pass, bytes.Repeat([]byte{0xbb}, 16))
	if err != nil {
		t.Fatalf("DeriveMasterKey(diff salt): %v", err)
	}
	if bytes.Equal(k1, k3) {
		t.Fatalf("salt 不同派生应不同")
	}
	// passphrase 不同 → 派生不同。
	k4, err := DeriveMasterKey([]byte("another passphrase"), salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey(diff pass): %v", err)
	}
	if bytes.Equal(k1, k4) {
		t.Fatalf("passphrase 不同派生应不同")
	}
}

// TestMasterKeyFromBase64_ValidAndInvalid 验证 base64 解码 32B master key：合法输入
// 返回 32B；非法输入（非 base64 / 非 32B）报错。
func TestMasterKeyFromBase64_ValidAndInvalid(t *testing.T) {
	key := bytes.Repeat([]byte{0xc3}, 32)
	b64 := base64.StdEncoding.EncodeToString(key)
	got, err := MasterKeyFromBase64(b64)
	if err != nil {
		t.Fatalf("MasterKeyFromBase64: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("解码结果不一致")
	}
	// 尾部换行/空白容忍（openssl rand -base64 输出带换行）。
	got2, err := MasterKeyFromBase64(b64 + "\n")
	if err != nil || !bytes.Equal(got2, key) {
		t.Fatalf("带换行的 base64 应容忍: got %x, err %v", got2, err)
	}
	if _, err := MasterKeyFromBase64("!!not-base64!!"); err == nil {
		t.Fatal("非法 base64 应报错")
	}
	if _, err := MasterKeyFromBase64(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("非 32B 解码结果应报错")
	}
}

// TestLoadMasterKeyFromFile_Base64AndRaw 验证 master key 文件的两种格式都接受：
// base64 32B（含尾换行）与 raw 32B 字节；非法内容报错。
func TestLoadMasterKeyFromFile_Base64AndRaw(t *testing.T) {
	key := bytes.Repeat([]byte{0x7e}, 32)

	t.Run("base64-带换行", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "master.key")
		content := base64.StdEncoding.EncodeToString(key) + "\n"
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadMasterKeyFromFile(p)
		if err != nil {
			t.Fatalf("LoadMasterKeyFromFile: %v", err)
		}
		if !bytes.Equal(got, key) {
			t.Fatalf("base64 格式解码不一致")
		}
	})

	t.Run("raw-32B", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "master.key")
		if err := os.WriteFile(p, key, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadMasterKeyFromFile(p)
		if err != nil {
			t.Fatalf("LoadMasterKeyFromFile(raw): %v", err)
		}
		if !bytes.Equal(got, key) {
			t.Fatalf("raw 格式读取不一致")
		}
	})

	t.Run("raw-末字节为换行0x0a不剥", func(t *testing.T) {
		// I-1 回归：合法 raw 32B key 的末字节恰为 0x0a（换行字节），整文件即密钥，
		// 不得按尾换行剥离（旧 TrimSpace 实现会误剥导致 31B 误拒）。
		raw := bytes.Repeat([]byte{0x5a}, 32)
		raw[31] = 0x0a
		p := filepath.Join(t.TempDir(), "master.key")
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadMasterKeyFromFile(p)
		if err != nil {
			t.Fatalf("LoadMasterKeyFromFile(raw-end-0x0a): %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("raw 末字节 0x0a 不应剥离: got %x", got)
		}
	})

	t.Run("raw-首字节为空格0x20不剥", func(t *testing.T) {
		// I-1 回归：raw key 首字节为合法空白字节 0x20，不得被 TrimSpace 误剥。
		raw := bytes.Repeat([]byte{0x24}, 32)
		raw[0] = 0x20
		p := filepath.Join(t.TempDir(), "master.key")
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadMasterKeyFromFile(p)
		if err != nil {
			t.Fatalf("LoadMasterKeyFromFile(raw-lead-space): %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("raw 首字节 0x20 不应剥离: got %x", got)
		}
	})

	t.Run("raw-32B加尾LF", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "master.key")
		content := append(append([]byte(nil), key...), '\n')
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadMasterKeyFromFile(p)
		if err != nil {
			t.Fatalf("LoadMasterKeyFromFile(raw+LF): %v", err)
		}
		if !bytes.Equal(got, key) {
			t.Fatalf("raw 32B+LF 应剥尾换行还原密钥: got %x", got)
		}
	})

	t.Run("raw-32B加尾CRLF", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "master.key")
		content := append(append(append([]byte(nil), key...), '\r'), '\n')
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadMasterKeyFromFile(p)
		if err != nil {
			t.Fatalf("LoadMasterKeyFromFile(raw+CRLF): %v", err)
		}
		if !bytes.Equal(got, key) {
			t.Fatalf("raw 32B+CRLF 应剥尾换行还原密钥: got %x", got)
		}
	})

	t.Run("非法内容", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "master.key")
		if err := os.WriteFile(p, []byte("not-a-key"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMasterKeyFromFile(p); err == nil {
			t.Fatal("非法 master key 文件应报错")
		}
	})

	t.Run("文件不存在", func(t *testing.T) {
		if _, err := LoadMasterKeyFromFile(filepath.Join(t.TempDir(), "nope.key")); err == nil {
			t.Fatal("缺失 master key 文件应报错")
		}
	})
}

// TestEncryptWithKey_NonceUnique 验证同明文 + 同 key 两次加密的密文不同（nonce 每次
// 随机，防 IV 复用回归——若 nonce 复用，同明文两次加密产物相同，攻击者可据密文相等
// 性推断明文关系）。
func TestEncryptWithKey_NonceUnique(t *testing.T) {
	key := bytes.Repeat([]byte{0x6a}, 32)
	payload := []byte(`{"version":1,"keys":[]}`)
	sealed1, err := EncryptWithKey(key, payload)
	if err != nil {
		t.Fatalf("EncryptWithKey(first): %v", err)
	}
	sealed2, err := EncryptWithKey(key, payload)
	if err != nil {
		t.Fatalf("EncryptWithKey(second): %v", err)
	}
	if len(sealed1) != len(sealed2) {
		t.Fatalf("两次密文长度应一致: %d vs %d", len(sealed1), len(sealed2))
	}
	if bytes.Equal(sealed1, sealed2) {
		t.Fatalf("同明文同 key 两次加密密文应不同（nonce 复用回归）")
	}
}

// TestDecryptWithKey_ShortCiphertextNoPanic 验证密文长度恰为 NonceSize()（12）或
// 12+1..12+15（不足 GCM tag 16B）时 DecryptWithKey 返回 error 而非 panic（防坏格式
// 输入触发 gcm.Open 越界/panic 回归）。
func TestDecryptWithKey_ShortCiphertextNoPanic(t *testing.T) {
	key := bytes.Repeat([]byte{0x3c}, 32)
	for n := 12; n <= 27; n++ {
		data := bytes.Repeat([]byte{0x00}, n)
		if _, err := DecryptWithKey(key, data); err == nil {
			t.Fatalf("密文长度 %d（不足 nonce+tag）Decrypt 应返回 error", n)
		}
	}
	// 边界：恰好 nonce+tag（28B）不 panic，但随机内容认证失败返回 error。
	full := bytes.Repeat([]byte{0x00}, 12+16)
	if _, err := DecryptWithKey(key, full); err == nil {
		t.Fatalf("随机 28B 密文认证应失败")
	}
}

// TestAESGCMStorer_ImplementsSecureStorer 编译期 + 行为断言：AESGCMStorer 满足
// SecureStorer 且往返等价（Encrypt 非明文 → Decrypt 还原）。
func TestAESGCMStorer_ImplementsSecureStorer(t *testing.T) {
	var _ SecureStorer = AESGCMStorer{}
	s := AESGCMStorer{Key: bytes.Repeat([]byte{0x77}, 32)}
	payload := []byte(`{"version":1,"keys":[]}`)
	ct, err := s.Encrypt(payload)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, payload) || strings.Contains(string(ct), "version") {
		t.Fatalf("AESGCMStorer.Encrypt 输出应为密文，非明文")
	}
	pt, err := s.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, payload) {
		t.Fatalf("AESGCMStorer 往返不一致")
	}
}

// TestLooksLikePlaintextJSON 表驱动锁定明文嗅探判定（M-1，encrypting_storer.go
// looksLikePlaintextJSON）——该分支决定 Load 解密失败时给「明文未迁移」定向提示还是
// 笼统「密文被篡改/key 错」，改坏恒返 false 会让 LoadPlaintextFailsClosed 误判。
func TestLooksLikePlaintextJSON(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "明文JSON首{含keys",
			data: []byte("{\n  \"version\": 1,\n  \"keys\": []\n}"),
			want: true,
		},
		{
			name: "明文JSON含keys无尾换行",
			data: []byte(`{"version":1,"keys":[{"ak":"x"}]}`),
			want: true,
		},
		{
			name: "首{但不含keys字段",
			data: []byte(`{"version":1,"entries":[]}`),
			want: false,
		},
		{
			name: "随机密文字节首非{",
			data: bytes.Repeat([]byte{0xa3}, 200),
			want: false,
		},
		{
			name: "随机密文恰首{但无keys",
			data: append([]byte{'{'}, bytes.Repeat([]byte{0x5c}, 64)...),
			want: false,
		},
		{
			name: "空输入",
			data: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikePlaintextJSON(tc.data); got != tc.want {
				t.Fatalf("looksLikePlaintextJSON = %v, want %v", got, tc.want)
			}
		})
	}
}

// recordingSecureStorer 是 SecureStorer 的计数探针（M-3）：委托 Encrypt/Decrypt 时计数，
// 用于锁定 EncryptingStorer 的委托契约——加密态 Save 恰 Encrypt=1、Load 恰 Decrypt=1，
// 明文态（secure=nil）不委托任何 SecureStorer。
type recordingSecureStorer struct {
	encryptCalls int
	decryptCalls int
}

func (r *recordingSecureStorer) Encrypt(p []byte) ([]byte, error) {
	r.encryptCalls++
	return append([]byte(nil), p...), nil
}

func (r *recordingSecureStorer) Decrypt(c []byte) ([]byte, error) {
	r.decryptCalls++
	return append([]byte(nil), c...), nil
}

// TestEncryptingStorer_DelegateCountContract 验证 EncryptingStorer 对 SecureStorer 的委托
// 计数契约（计划任务 2 步骤 1「mock SecureStorer 断言 Save 前调 Encrypt」的落地）：
//   - 加密态 Save 恰好 Encrypt=1（不重复加密）、Load 恰好 Decrypt=1（不重复解密）；
//   - 明文态（secure=nil）不经 SecureStorer——磁盘保持明文 JSON 即证明未调用 Encrypt。
func TestEncryptingStorer_DelegateCountContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant", "meta", "credentials.json")
	rec := &recordingSecureStorer{}
	st := NewEncryptingStorer(path, rec)

	if err := st.Save(sampleEncryptingKeys()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if rec.encryptCalls != 1 || rec.decryptCalls != 0 {
		t.Fatalf("加密态 Save 应恰 Encrypt=1 Decrypt=0, got Encrypt=%d Decrypt=%d", rec.encryptCalls, rec.decryptCalls)
	}
	if _, err := st.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.encryptCalls != 1 || rec.decryptCalls != 1 {
		t.Fatalf("加密态 Load 应恰 Encrypt=1 Decrypt=1, got Encrypt=%d Decrypt=%d", rec.encryptCalls, rec.decryptCalls)
	}

	// 明文态（secure=nil）：不经 SecureStorer（Save 直写明文 JSON、Load 直读）——
	// secure 为 nil 无计数对象，以「磁盘保持明文 JSON（含 \"keys\"）」作为未调用 Encrypt
	// 的可观察证据（等价 Encrypt/Decrypt 调用 = 0）。
	plainPath := filepath.Join(dir, "plain", "credentials.json")
	plainSt := NewEncryptingStorer(plainPath, nil)
	if err := plainSt.Save(sampleEncryptingKeys()); err != nil {
		t.Fatalf("plain Save: %v", err)
	}
	disk, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(disk, []byte(`"keys"`)) {
		t.Fatalf("明文态磁盘应保持明文 JSON（未调用 Encrypt）")
	}
	if _, err := plainSt.Load(); err != nil {
		t.Fatalf("plain Load: %v", err)
	}
}
