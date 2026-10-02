// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"encoding/binary"
	"testing"

	"golang.org/x/crypto/scrypt"
)

// TestEncryptBlock_UniformOnDiskFormat 验证分块统一落盘格式
// [R 128B][4B 密文长 BE][salt][nonce][ct+tag]：R 段随机、长度头与文件大小线性一致、
// roundtrip 可用、篡改 R 段不影响解密（R 仅混淆，不校验）。
func TestEncryptBlock_UniformOnDiskFormat(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x11}, KeyLen)
	salt := bytes.Repeat([]byte{0x22}, SaltLen)
	blob, err := encryptBlock(key, salt, []byte("hello"))
	if err != nil {
		t.Fatalf("encryptBlock: %v", err)
	}
	// 格式 [R 128B][4B 密文长][salt][nonce][ct+tag]
	if len(blob) != RandPrefixLen+4+SaltLen+NonceLen+len("hello")+16 {
		t.Errorf("blob 长度 %d 不符 R+4+salt+nonce+ct+tag", len(blob))
	}
	// 长度头 = 密文长度 = len(blob) - R - 4 - 32 - 12
	ctLen := binary.BigEndian.Uint32(blob[RandPrefixLen : RandPrefixLen+4])
	if int(ctLen) != len(blob)-RandPrefixLen-4-SaltLen-NonceLen {
		t.Errorf("长度头 %d 与文件大小线性关系不符", ctLen)
	}
	// R 段内容随机（非全 0/全 1）
	if bytes.Equal(blob[:RandPrefixLen], bytes.Repeat([]byte{0}, RandPrefixLen)) {
		t.Error("R 段不应全零")
	}
	// 解密 roundtrip
	plain, err := decryptBlock(key, salt, blob)
	if err != nil || string(plain) != "hello" {
		t.Errorf("decryptBlock roundtrip: %v", err)
	}
	// 篡改 R 段不影响解密
	badR := append([]byte(nil), blob...)
	badR[0] ^= 0xFF
	if _, err := decryptBlock(key, salt, badR); err != nil {
		t.Errorf("篡改 R 段不应影响解密：%v", err)
	}
}

// TestEncryptMetaJSON_PadLengthInRange：padTarget=0 = 不 padding（纯密文长度），
// 长度头与文件大小线性一致；解密精确还原（jsonLen 截取）；篡改密文段 fail-closed。
func TestEncryptMetaJSON_PadLengthInRange(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x33}, KeyLen)
	salt := bytes.Repeat([]byte{0x44}, SaltLen)
	metaJSON := []byte(`{"version":1}`)
	blob, err := encryptMetaJSON(key, salt, metaJSON, 0)
	if err != nil {
		t.Fatalf("encryptMetaJSON: %v", err)
	}
	// 无 padding 时：明文 = [4B jsonLen][JSON]，密文长 = len(明文)+16
	plainLen := 4 + len(metaJSON)
	if len(blob) != RandPrefixLen+4+SaltLen+NonceLen+plainLen+16 {
		t.Errorf("无 padding blob 长度 %d 不符", len(blob))
	}
	// 解密精确还原（json 截取，padding 不进入 JSON）
	got, err := decryptMetaJSON(key, blob)
	if err != nil || string(got) != string(metaJSON) {
		t.Errorf("decryptMetaJSON: %v", err)
	}
	// 篡改密文段（padding 属 GCM 认证范围）必失败
	bad := append([]byte(nil), blob...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := decryptMetaJSON(key, bad); err == nil {
		t.Error("篡改密文应 fail-closed")
	}
}

// TestEncryptMetaJSON_PadToTarget：padTarget>0 时 padding 到「整块总长 = padTarget」，
// 落在 [R+4+salt+nonce+tag下限, +∞] 范围内；解密、restored 原 JSON，padding 不进 JSON。
func TestEncryptMetaJSON_PadToTarget(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x55}, KeyLen)
	salt := bytes.Repeat([]byte{0x66}, SaltLen)
	metaJSON := []byte(`{"a":"b"}`)
	// 说明：padTarget 为「整块落盘总长」目标。最小可能尺寸 = R(128)+4B+盐(32)+nonce(12)+
	// 最小 tag(16) = 192B，故 128 不可行（简报取值 128，仅 R 首部就占满）；此处取 512
	// 作为可行目标，语义与简报一致（padding 到指定总长）。
	const padTarget = 512
	blob, err := encryptMetaJSON(key, salt, metaJSON, padTarget)
	if err != nil {
		t.Fatalf("encryptMetaJSON: %v", err)
	}
	if len(blob) != padTarget {
		t.Errorf("padding 后 blob 长度 %d，应为目标 %d", len(blob), padTarget)
	}
	got, err := decryptMetaJSON(key, blob)
	if err != nil || string(got) != string(metaJSON) {
		t.Errorf("padding 后解密应还原原 JSON：%v", err)
	}
}

func TestEncryptMetaJSON_TooSmallTargetNoExpand(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x77}, KeyLen)
	salt := bytes.Repeat([]byte{0x88}, SaltLen)
	metaJSON := []byte(`{"x":1}`)
	// padTarget 小于天然总长时视为「不 padding」（padding 只往大里扩，绝不裁剪）。
	blob, err := encryptMetaJSON(key, salt, metaJSON, RandPrefixLen)
	if err != nil {
		t.Fatalf("encryptMetaJSON: %v", err)
	}
	plainLen := 4 + len(metaJSON)
	if len(blob) != RandPrefixLen+4+SaltLen+NonceLen+plainLen+16 {
		t.Errorf("过小 padTarget 不应裁剪 blob：len=%d", len(blob))
	}
}

// TestDeriveKey_VersionDomainSeparation 验证算法版本 KDF 域分离：secret+salt 相同、
// 派生域不同 → 派生 key 不同（算法版本经 KDF 派生域混入 secret，域不同 key 不同）；
// v1 域标记 = "shardseal/v1"（显式域，功能未上线无旧 blob 兼容垫）；未知版本 fail-closed。
func TestDeriveKey_VersionDomainSeparation(t *testing.T) {
	t.Parallel()
	secret := []byte("super-secret-32-bytes")
	salt := bytes.Repeat([]byte{0x42}, SaltLen)

	// v1 域标记正确：registry 登记 "shardseal/v1"，deriveKey(v1) == 该 material 的 scrypt。
	v1Alg, ok := registry[AlgoV1GCM]
	if !ok {
		t.Fatal("AlgoV1GCM 应已注册")
	}
	if v1Alg.KDFDomain != "shardseal/v1" {
		t.Errorf("v1 KDF 派生域=%q，应为 %q", v1Alg.KDFDomain, "shardseal/v1")
	}
	v1Key, err := deriveKey(secret, salt, AlgoV1GCM)
	if err != nil {
		t.Fatalf("deriveKey(v1): %v", err)
	}
	wantV1, err := scrypt.Key(kdfMaterial(secret, v1Alg.KDFDomain), salt, scryptN, scryptR, scryptP, KeyLen)
	if err != nil {
		t.Fatalf("scrypt(v1 域): %v", err)
	}
	if !bytes.Equal(v1Key, wantV1) {
		t.Error("deriveKey(v1) 应等于 secret||\"shardseal/v1\" 的 scrypt（域标记混入派生）")
	}

	// 派生域分离：域不同 → key 不同（同 secret+salt）。域经 kdfMaterial 混入 secret，
	// 是「算法版本进派生输入、不明文进 blob」的机制本身。
	otherKey, err := scrypt.Key(kdfMaterial(secret, "v2-other-domain"), salt, scryptN, scryptR, scryptP, KeyLen)
	if err != nil {
		t.Fatalf("scrypt(域分离): %v", err)
	}
	if bytes.Equal(v1Key, otherKey) {
		t.Error("KDF 派生域不同应产生不同 key（域分离缺失）")
	}
}

// TestDeriveKey_UnknownVersionFails：未注册算法版本派生 fail-closed（无法确定派生域）。
func TestDeriveKey_UnknownVersionFails(t *testing.T) {
	t.Parallel()
	if _, err := deriveKey([]byte("secret"), make([]byte, SaltLen), AlgoVersion(99)); err == nil {
		t.Fatal("未知算法版本派生应失败（fail-closed），却成功")
	}
}

// TestRegisterAlgorithm_DuplicatePanic：重复版本注册 fail-fast panic（杜绝旧 blob
// 解密视图漂移）。AlgoV1GCM 已由 init 装配，重复注册必 panic。
func TestRegisterAlgorithm_DuplicatePanic(t *testing.T) {
	t.Parallel()
	defer func() { _ = recover() }()
	RegisterAlgorithm(Algorithm{Version: AlgoV1GCM})
	t.Fatal("重复注册 AlgoV1GCM 应 panic，却未 panic")
}
