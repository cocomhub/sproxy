// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIdentityGenerate_Basic 验证生成的身份：公钥 32B、指纹为 sha256:<64 hex>。
func TestIdentityGenerate_Basic(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	if id == nil {
		t.Fatal("identity is nil")
	}
	pub := id.PublicKey()
	if len(pub) != 32 {
		t.Fatalf("expected 32-byte public key, got %d", len(pub))
	}
	fp := id.Fingerprint()
	if !strings.HasPrefix(fp, fingerprintPrefix) {
		t.Fatalf("fingerprint %q missing prefix %q", fp, fingerprintPrefix)
	}
	if len(fp) != len(fingerprintPrefix)+sha256HexLen {
		t.Fatalf("fingerprint %q has wrong length (len=%d)", fp, len(fp))
	}
	hexPart := fp[len(fingerprintPrefix):]
	if !isLowerHex(hexPart) {
		t.Fatalf("fingerprint hex not lowercase hex: %q", hexPart)
	}
}

// TestIdentityGenerate_Unique 验证两次生成的身份指纹不同。
func TestIdentityGenerate_Unique(t *testing.T) {
	id1, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if id1.Fingerprint() == id2.Fingerprint() {
		t.Fatal("two generated identities have identical fingerprint")
	}
}

// TestFingerprintFromPublicKey_Deterministic 验证同公钥指纹确定。
func TestFingerprintFromPublicKey_Deterministic(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pub := id.PublicKey()
	fp1 := FingerprintFromPublicKey(pub)
	fp2 := FingerprintFromPublicKey(pub)
	if fp1 != fp2 {
		t.Fatalf("fingerprint not deterministic: %q vs %q", fp1, fp2)
	}
	if !strings.HasPrefix(fp1, fingerprintPrefix) {
		t.Fatalf("expected fingerprint prefix, got %q", fp1)
	}
}

// TestIdentitySaveLoad_Roundtrip 验证保存后加载指纹一致。
func TestIdentitySaveLoad_Roundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveIdentity(id, path); err != nil {
		t.Fatalf("SaveIdentity: %v", err)
	}
	loaded, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if loaded.Fingerprint() != id.Fingerprint() {
		t.Fatalf("fingerprint mismatch after roundtrip: %s vs %s", loaded.Fingerprint(), id.Fingerprint())
	}
}

// TestIdentitySave_Perm0600 验证身份文件权限 0600（Windows chmod 是 no-op，跳过）。
func TestIdentitySave_Perm0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Chmod 在 Windows 上是 no-op，权限位不可靠")
	}
	path := filepath.Join(t.TempDir(), "identity.json")
	id, _ := GenerateIdentity()
	if err := SaveIdentity(id, path); err != nil {
		t.Fatalf("SaveIdentity: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != identityFilePerm {
		t.Fatalf("expected perm %o, got %o", identityFilePerm, perm)
	}
}

// TestIdentitySave_CreatesDirs 验证保存时自动创建父目录。
func TestIdentitySave_CreatesDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "identity.json")
	id, _ := GenerateIdentity()
	if err := SaveIdentity(id, path); err != nil {
		t.Fatalf("SaveIdentity: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

// TestIdentitySave_OverwritesExisting 验证覆盖已有文件（原子替换）。
func TestIdentitySave_OverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	id1, _ := GenerateIdentity()
	id2, _ := GenerateIdentity()
	if err := SaveIdentity(id1, path); err != nil {
		t.Fatal(err)
	}
	if err := SaveIdentity(id2, path); err != nil {
		t.Fatalf("second SaveIdentity: %v", err)
	}
	loaded, err := LoadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Fingerprint() != id2.Fingerprint() {
		t.Fatalf("expected id2 fingerprint after overwrite, got %s", loaded.Fingerprint())
	}
}

// TestIdentityLoad_NotExist 验证加载不存在的文件返回 IsNotExist。
func TestIdentityLoad_NotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	_, err := LoadIdentity(path)
	if !os.IsNotExist(err) {
		t.Fatalf("expected IsNotExist, got %v", err)
	}
}

// TestIdentityLoad_CorruptJSON 验证损坏 JSON 返回 ErrIdentityFileCorrupt（fail-closed，不静默重建）。
func TestIdentityLoad_CorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadIdentity(path)
	if err == nil {
		t.Fatal("expected error for corrupt json")
	}
	if !errors.Is(err, ErrIdentityFileCorrupt) {
		t.Fatalf("expected ErrIdentityFileCorrupt, got %v", err)
	}
}

// TestIdentityLoad_BadPrivateKey 验证私钥非法的身份文件返回 ErrIdentityFileCorrupt。
func TestIdentityLoad_BadPrivateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	data := []byte(`{"private_key":"zz","public_key":"zz"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadIdentity(path)
	if !errors.Is(err, ErrIdentityFileCorrupt) {
		t.Fatalf("expected ErrIdentityFileCorrupt, got %v", err)
	}
}

// TestIdentityLoad_ShortPrivateKey 验证私钥长度非 32B 返回 ErrIdentityFileCorrupt。
func TestIdentityLoad_ShortPrivateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	data := []byte(`{"private_key":"abcdef","public_key":"abcdef"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadIdentity(path)
	if !errors.Is(err, ErrIdentityFileCorrupt) {
		t.Fatalf("expected ErrIdentityFileCorrupt, got %v", err)
	}
}

// TestIdentityLoad_KeyMismatch 验证公钥与私钥派生不一致返回 ErrIdentityFileCorrupt。
func TestIdentityLoad_KeyMismatch(t *testing.T) {
	id1, _ := GenerateIdentity()
	id2, _ := GenerateIdentity()
	data := []byte(`{"private_key":"` + hex.EncodeToString(id1.privateKey.Seed()) +
		`","public_key":"` + hex.EncodeToString(id2.PublicKey()) + `"}`)
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadIdentity(path)
	if !errors.Is(err, ErrIdentityFileCorrupt) {
		t.Fatalf("expected ErrIdentityFileCorrupt, got %v", err)
	}
}

// TestLoadOrCreateIdentity 验证不存在时创建、存在时复用。
func TestLoadOrCreateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	id1, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	id2, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("LoadOrCreate second: %v", err)
	}
	if id1.Fingerprint() != id2.Fingerprint() {
		t.Fatal("LoadOrCreate should reuse existing identity")
	}
}

// TestLoadOrCreateIdentity_Corrupt 验证已存在但损坏的文件直接报错（不覆盖）。
func TestLoadOrCreateIdentity_Corrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(path, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateIdentity(path)
	if !errors.Is(err, ErrIdentityFileCorrupt) {
		t.Fatalf("expected ErrIdentityFileCorrupt, got %v", err)
	}
}

// TestParseFingerprint 验证指纹输入归一化：接受纯 hex、sha256: 前缀、大写 hex，拒绝非法。
func TestParseFingerprint(t *testing.T) {
	id, _ := GenerateIdentity()
	fp := id.Fingerprint()
	hexOnly := strings.TrimPrefix(fp, fingerprintPrefix)

	parsed, err := ParseFingerprint(hexOnly)
	if err != nil {
		t.Fatalf("ParseFingerprint(hex): %v", err)
	}
	if parsed != fp {
		t.Fatalf("expected %q, got %q", fp, parsed)
	}

	parsed, err = ParseFingerprint(fp)
	if err != nil {
		t.Fatalf("ParseFingerprint(sha256:): %v", err)
	}
	if parsed != fp {
		t.Fatalf("expected %q, got %q", fp, parsed)
	}

	// 大写 hex 应归一化小写
	parsed, err = ParseFingerprint(strings.ToUpper(hexOnly))
	if err != nil {
		t.Fatalf("ParseFingerprint(upper): %v", err)
	}
	if parsed != fp {
		t.Fatalf("upper not normalized to lower: %q", parsed)
	}

	// 带空白
	parsed, err = ParseFingerprint("  " + fp + "\n")
	if err != nil {
		t.Fatalf("ParseFingerprint(whitespace): %v", err)
	}
	if parsed != fp {
		t.Fatalf("expected %q, got %q", fp, parsed)
	}

	for _, bad := range []string{
		"",
		"short",
		strings.Repeat("g", 64),  // 非 hex
		fingerprintPrefix + "zz", // 前缀后非 hex
		strings.Repeat("a", 63),  // 长度不足
		fingerprintPrefix + strings.Repeat("a", 65), // 长度过长
	} {
		if _, err := ParseFingerprint(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

// TestFingerprintMatches 验证指纹恒时比较。
func TestFingerprintMatches(t *testing.T) {
	id, _ := GenerateIdentity()
	fp := id.Fingerprint()
	if !FingerprintMatches(fp, fp) {
		t.Fatal("fingerprint should match itself")
	}
	if FingerprintMatches(fp, strings.Repeat("a", len(fp))) {
		t.Fatal("fingerprint should not match different value")
	}
	if FingerprintMatches(fp, fp+"x") {
		t.Fatal("different lengths should not match")
	}
}

// TestIdentity_SignVerify 验证身份签名/验签（proof of possession）：
// 自身签名可验证；他人公钥或篡改消息验签失败。
func TestIdentity_SignVerify(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("sproxy-identity-v1-test-message")
	sig := id.Sign(msg)
	if len(sig) != ed25519SignatureLen {
		t.Fatalf("signature length = %d, want %d", len(sig), ed25519SignatureLen)
	}
	if !id.Verify(msg, sig) {
		t.Fatal("Verify should pass for own signature")
	}

	other, _ := GenerateIdentity()
	if other.Verify(msg, sig) {
		t.Fatal("Verify should fail for wrong public key (no possession)")
	}
	if id.Verify([]byte("tampered-message"), sig) {
		t.Fatal("Verify should fail for tampered message")
	}
	if id.Verify(msg, []byte("bad-signature")) {
		t.Fatal("Verify should fail for garbage signature")
	}
}

// TestIdentity_SignNil 验证空身份 Sign/Verify 返回安全默认（nil/false），不 panic。
func TestIdentity_SignNil(t *testing.T) {
	var nilID *Identity
	if sig := nilID.Sign([]byte("x")); sig != nil {
		t.Fatalf("expected nil signature, got %v", sig)
	}
	if nilID.Verify([]byte("x"), []byte("sig")) {
		t.Fatal("expected false for nil identity verify")
	}
}

// TestPinContains 验证 pin 列表匹配（含归一化）。
func TestPinContains(t *testing.T) {
	id, _ := GenerateIdentity()
	fp := id.Fingerprint()
	hexOnly := strings.TrimPrefix(fp, fingerprintPrefix)

	if !pinContains([]string{fp}, fp) {
		t.Fatal("exact fingerprint should match")
	}
	if !pinContains([]string{hexOnly}, fp) {
		t.Fatal("hex-only pin should match after normalization")
	}
	if !pinContains([]string{strings.ToUpper(hexOnly)}, fp) {
		t.Fatal("upper-case pin should match after normalization")
	}
	if pinContains([]string{}, fp) {
		t.Fatal("empty pin list should not match")
	}
	if pinContains(nil, fp) {
		t.Fatal("nil pin list should not match")
	}
	other, _ := GenerateIdentity()
	if pinContains([]string{other.Fingerprint()}, fp) {
		t.Fatal("unrelated fingerprint should not match")
	}
}

func isLowerHex(s string) bool {
	if len(s) == 0 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
