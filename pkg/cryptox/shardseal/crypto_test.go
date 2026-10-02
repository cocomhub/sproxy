// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"encoding/binary"
	"testing"

	"golang.org/x/crypto/scrypt"
)

// TestEncryptBlocklets_UniformOnDiskFormat 验证最终定稿块 blob 结构
// [R 128B][8B 密文流总长 BE][salt][boot][index][段...]：8B 总长 = 密文流长、R 段随机、
// boot 位于固定偏移、全量解密 roundtrip、篡改 R 不影响、篡改密文 fail-closed。
func TestEncryptBlocklets_UniformOnDiskFormat(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x11}, KeyLen)
	salt := bytes.Repeat([]byte{0x22}, SaltLen)
	blocklets := []Blocklet{{Offset: 0, Size: 8}, {Offset: 8, Size: 16}, {Offset: 24, Size: 8}}
	data := bytes.Repeat([]byte{0xA5}, 32)
	blob, entries, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	// [R][8B 密文流总长]：总长 == len(blob) - (R + 8 + salt)。
	if got := binary.BigEndian.Uint64(blob[RandPrefixLen : RandPrefixLen+hdrLen]); int(got) != len(blob)-blListOff {
		t.Errorf("密文流总长 %d，应为 %d", got, len(blob)-blListOff)
	}
	// R 段随机（非全 0/全 1）。
	if bytes.Equal(blob[:RandPrefixLen], bytes.Repeat([]byte{0}, RandPrefixLen)) {
		t.Error("R 段不应全零")
	}
	// 索引条目数与 blocklet 数一致，enc_size/enc_offset 自洽（boot 位于固定偏移 blListOff）。
	if len(entries) != len(blocklets) {
		t.Fatalf("索引条目数=%d，应为 %d", len(entries), len(blocklets))
	}
	for i, e := range entries {
		if e.EncSize != blockletEncSize(blocklets[i].Size) {
			t.Errorf("entries[%d].enc_size=%d，应为 %d", i, e.EncSize, blockletEncSize(blocklets[i].Size))
		}
		if e.EncOffset < blListOff {
			t.Errorf("entries[%d].enc_offset=%d 越出密文流起点", i, e.EncOffset)
		}
	}
	// 全量解密 roundtrip == 原始块明文。
	plain, err := decryptBlock(key, salt, blob)
	if err != nil || !bytes.Equal(plain, data) {
		t.Errorf("decryptBlock roundtrip: err=%v len=%d", err, len(plain))
	}
	// 篡改 R 段不影响解密（R 仅混淆）。
	badR := append([]byte(nil), blob...)
	badR[0] ^= 0xFF
	if _, err := decryptBlock(key, salt, badR); err != nil {
		t.Errorf("篡改 R 段不应影响解密：%v", err)
	}
	// 篡改密文 fail-closed（GCM 认证）。
	bad := append([]byte(nil), blob...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := decryptBlock(key, salt, bad); err == nil {
		t.Error("篡改密文应 fail-closed")
	}
}

// TestEncryptBlocklets_Roundtrip 多 blocklet 块加解密还原：随机规划 blocklet 切分一块，
// 全量解密 == 原明文；错误密钥 fail-closed；blocklet 偏移超 uint32 fail-closed。
func TestEncryptBlocklets_Roundtrip(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x33}, KeyLen)
	salt := bytes.Repeat([]byte{0x44}, SaltLen)
	p := &FixedBlockletPlanner{Min: 4, Max: 16}
	blocklets, err := p.PlanBlocklets(nil, 500, 0, 500)
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	data := make([]byte, 500)
	for i := range data {
		data[i] = byte(i % 251)
	}
	blob, _, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	got, err := decryptBlock(key, salt, blob)
	if err != nil {
		t.Fatalf("decryptBlock: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("roundtrip 内容不一致：len(got)=%d len(want)=%d", len(got), len(data))
	}
	// 错误密钥 fail-closed。
	if _, err := decryptBlock(bytes.Repeat([]byte{0x99}, KeyLen), salt, blob); err == nil {
		t.Error("错误密钥解密应失败")
	}
	// 相对偏移超过 uint32 上限 → 加密 fail-closed（不静默截断 AAD 内 off）。
	huge := []Blocklet{{Offset: 0, Size: 8}, {Offset: 1 << 32, Size: 8, Padding: true}}
	if _, _, err := encryptBlocklets(key, salt, huge, make([]byte, 8), AlgoV1GCM); err == nil {
		t.Error("blocklet 相对偏移超 uint32 应 fail-closed")
	}
}

// TestDecryptBlockletAt_RangeRead 验证无 meta 随机访问：解 boot→index 定位含目标偏移的
// 数据段并只解该段（不下载/解密其它 blocklet）。回归断言：返回所在 blocklet 内容而非整块。
func TestDecryptBlockletAt_RangeRead(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x55}, KeyLen)
	salt := bytes.Repeat([]byte{0x66}, SaltLen)
	// 块内 4 个 blocklet 各 32B → [0,128)。
	blocklets := []Blocklet{
		{Offset: 0, Size: 32}, {Offset: 32, Size: 32}, {Offset: 64, Size: 32}, {Offset: 96, Size: 32},
	}
	data := make([]byte, 128)
	for i := range data {
		data[i] = byte(i)
	}
	blob, _, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	// 目标 100 位于第四个 blocklet [96,128)。只应返回该 blocklet 明文。
	const target = int64(100)
	got, bl, err := decryptBlockletAt(key, salt, blob, target)
	if err != nil {
		t.Fatalf("decryptBlockletAt: %v", err)
	}
	if bl.Offset != 96 || bl.Size != 32 {
		t.Errorf("blocklet 描述 = %+v，应为 {96 32}", bl)
	}
	wantSeg := data[bl.Offset : bl.Offset+bl.Size]
	if !bytes.Equal(got, wantSeg) {
		t.Errorf("只应解出所在 blocklet：len(got)=%d，应为 %d（非整块 %d）", len(got), len(wantSeg), len(data))
	}
	// 目标偏移在块外 → fail-closed。
	if _, _, err := decryptBlockletAt(key, salt, blob, 200); err == nil {
		t.Error("越界目标偏移应 fail-closed")
	}
	// salt 不一致（块被替换/错位）→ fail-closed。
	if _, _, err := decryptBlockletAt(key, bytes.Repeat([]byte{0x99}, SaltLen), blob, target); err == nil {
		t.Error("blocklet salt 不一致应 fail-closed")
	}
}

// TestDecryptBlock_ChunkSaltMismatch 验证块 blob 内 salt 与 expectSalt 不一致时 fail-closed
// （decryptBlock 校验，防块被替换/错位）。
func TestDecryptBlock_ChunkSaltMismatch(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x77}, KeyLen)
	salt := bytes.Repeat([]byte{0x88}, SaltLen)
	blob, _, err := encryptBlocklets(key, salt, []Blocklet{{Offset: 0, Size: 8}}, bytes.Repeat([]byte{7}, 8), AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	if _, err := decryptBlock(key, bytes.Repeat([]byte{0xFF}, SaltLen), blob); err == nil {
		t.Error("salt 不一致应 fail-closed")
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
	if len(blob) != RandPrefixLen+hdrLen+SaltLen+NonceLen+plainLen+16 {
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
	// 说明：padTarget 为「整块落盘总长」目标。最小可能尺寸 = R(128)+8B长度头+盐(32)+nonce(12)+
	// 最小 tag(16) = 196B，故 128 不可行（仅 R 首部就占满）；此处取 512
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
	if len(blob) != RandPrefixLen+hdrLen+SaltLen+NonceLen+plainLen+16 {
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

// TestXORParity_Roundtrip：k 个数据块（k=4，等长）→ XORParity → parity 块；
// 每块缺失都能用 RecoverFromParity(parity + 其余块) 恢复 == 原块（XOR 恒等式）。
func TestXORParity_Roundtrip(t *testing.T) {
	t.Parallel()
	blocks := [][]byte{
		[]byte("block-0000-AAAA"),
		[]byte("block-0000-BBBB"),
		[]byte("block-0000-CCCC"),
		[]byte("block-0000-DDDD"),
	}
	parity, err := XORParity(blocks...)
	if err != nil {
		t.Fatalf("XORParity: %v", err)
	}
	if len(parity) != len(blocks[0]) {
		t.Fatalf("parity 长度 %d，应为 %d", len(parity), len(blocks[0]))
	}
	// 逐块缺失都能恢复 == 原块。
	for missing := 0; missing < len(blocks); missing++ {
		others := make([][]byte, 0, len(blocks)-1)
		for i, b := range blocks {
			if i != missing {
				others = append(others, b)
			}
		}
		got, rerr := RecoverFromParity(parity, others, missing)
		if rerr != nil {
			t.Fatalf("RecoverFromParity(missing=%d): %v", missing, rerr)
		}
		if !bytes.Equal(got, blocks[missing]) {
			t.Errorf("missing=%d 恢复内容 != 原块", missing)
		}
	}
}

// TestRecover_MissingBlock：任一块缺失 → RecoverFromParity(parity + 其余 k-1 块) 还原
// == 原块；错误输入（parity 空 / 块长不一致 / missingIndex<0）fail-closed。
func TestRecover_MissingBlock(t *testing.T) {
	t.Parallel()
	blocks := [][]byte{
		[]byte("alpha-----0000000000"),
		[]byte("bravo-----0000000000"),
		[]byte("charlie---0000000000"),
	}
	parity, err := XORParity(blocks...)
	if err != nil {
		t.Fatalf("XORParity: %v", err)
	}
	missing := 1
	others := append([][]byte(nil), blocks[:missing]...)
	others = append(others, blocks[missing+1:]...)
	got, err := RecoverFromParity(parity, others, missing)
	if err != nil {
		t.Fatalf("RecoverFromParity: %v", err)
	}
	if !bytes.Equal(got, blocks[missing]) {
		t.Fatalf("恢复块 != 原块：got=%q", got)
	}

	// 错误输入 fail-closed。
	if _, err := RecoverFromParity(nil, others, missing); err == nil {
		t.Error("空 parity 应报错（fail-closed）")
	}
	badLen := make([][]byte, len(others))
	copy(badLen, others)
	badLen[0] = append(append([]byte(nil), others[0]...), 0x01)
	if _, err := RecoverFromParity(parity, badLen, missing); err == nil {
		t.Error("块长与 parity 不一致应报错")
	}
	if _, err := RecoverFromParity(parity, others, -1); err == nil {
		t.Error("missingIndex<0 应报错")
	}
}

// TestXORParity_InvalidInput：XORParity 输入校验 fail-closed（<2 块 / nil / 空 / 不等长）。
func TestXORParity_InvalidInput(t *testing.T) {
	t.Parallel()
	if _, err := XORParity([]byte("only-one")); err == nil {
		t.Error("单块 XORParity 应报错（无冗余意义）")
	}
	if _, err := XORParity(nil); err == nil {
		t.Error("nil 块应报错")
	}
	if _, err := XORParity([]byte{}, []byte{}); err == nil {
		t.Error("空块应报错")
	}
	if _, err := XORParity([]byte("abc"), []byte("abcd")); err == nil {
		t.Error("不等长块应报错")
	}
}
