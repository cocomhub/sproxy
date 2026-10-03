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
// [R 128B][8B 密文流总长 BE][salt][boot][段...][index]：8B 总长 = 密文流长、R 段随机、
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

// TestDeriveKey_VersionDomainSeparation 验证三档 KDF 版本化：standard/high/low 各注册为
// 唯一 Version + 唯一 KDF 域（"shardseal/v1"/"-high"/"-low"）+ 各自 scrypt 参数；同
// secret+salt 各档派生 key 互不相同（档位域分离）；各档 deriveKey 与「域+档参直算 scrypt」
// 一致。
func TestDeriveKey_VersionDomainSeparation(t *testing.T) {
	t.Parallel()
	secret := []byte("super-secret-32-bytes")
	salt := bytes.Repeat([]byte{0x42}, SaltLen)

	tiers := []struct {
		name     string
		ver      AlgoVersion
		domain   string
		wantName string
		wantN    int
	}{
		{"standard", AlgoV1GCM, "shardseal/v1", AlgorithmName, 1 << 14},
		{"high", AlgoV1GCMHigh, "shardseal/v1-high", AlgorithmName + "-high", 1 << 17},
		{"low", AlgoV1GCMLow, "shardseal/v1-low", AlgorithmName + "-low", 1 << 12},
	}
	keys := map[AlgoVersion][]byte{}
	for _, tc := range tiers {
		alg, ok := registry[tc.ver]
		if !ok {
			t.Fatalf("%s 档 %d 应已注册", tc.name, tc.ver)
		}
		if alg.Name != tc.wantName {
			t.Errorf("%s 档名=%q，应为 %q", tc.name, alg.Name, tc.wantName)
		}
		if alg.KDFDomain != tc.domain {
			t.Errorf("%s 档 KDF 域=%q，应为 %q", tc.name, alg.KDFDomain, tc.domain)
		}
		if alg.ScryptN != tc.wantN || alg.ScryptR != 8 || alg.ScryptP != 1 {
			t.Errorf("%s 档 scrypt 参数=(%d,%d,%d)，应为 (%d,8,1)", tc.name, alg.ScryptN, alg.ScryptR, alg.ScryptP, tc.wantN)
		}
		got, err := deriveKey(secret, salt, tc.ver)
		if err != nil {
			t.Fatalf("deriveKey(%s): %v", tc.name, err)
		}
		want, err := scrypt.Key(kdfMaterial(secret, alg.KDFDomain), salt, alg.ScryptN, alg.ScryptR, alg.ScryptP, KeyLen)
		if err != nil {
			t.Fatalf("scrypt(%s 域): %v", tc.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("deriveKey(%s) 应等于 secret||%q 的 scrypt（域标记混入派生）", tc.name, tc.domain)
		}
		keys[tc.ver] = got
	}
	// 三档派生域分离：同 secret+salt，各档 key 互不相同（域不同 → key 不同）。
	if bytes.Equal(keys[AlgoV1GCM], keys[AlgoV1GCMHigh]) ||
		bytes.Equal(keys[AlgoV1GCM], keys[AlgoV1GCMLow]) ||
		bytes.Equal(keys[AlgoV1GCMHigh], keys[AlgoV1GCMLow]) {
		t.Error("KDF 档位域分离缺失：不同档应派生不同 key（同 secret+salt）")
	}
}

// TestTier_RoundTripAndCrossTierFailClosed 验证档位 roundtrip 与跨档 fail-closed：
// 同 secret+salt，用某档派生 key 加密 → 同档 key 解密成功；用另一档派生 key 解密必失败
// （档位不同 → Version + KDF 域 + scrypt 参数不同 → key 不同 → GCM fail-closed）。
func TestTier_RoundTripAndCrossTierFailClosed(t *testing.T) {
	t.Parallel()
	secret := []byte("tier-secret-key-material")
	salt := bytes.Repeat([]byte{0x77}, SaltLen)
	plain := []byte("same tier roundtrip payload")
	tiers := []AlgoVersion{AlgoV1GCM, AlgoV1GCMHigh, AlgoV1GCMLow}

	keys := map[AlgoVersion][]byte{}
	blobs := map[AlgoVersion][]byte{}
	for _, v := range tiers {
		key, err := deriveKey(secret, salt, v)
		if err != nil {
			t.Fatalf("deriveKey(%d): %v", v, err)
		}
		keys[v] = key
		blob, err := sealBlock(key, salt, plain)
		if err != nil {
			t.Fatalf("sealBlock(%d): %v", v, err)
		}
		blobs[v] = blob
		// 同档 roundtrip 成功。
		got, err := openBlock(key, blob)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("档 %d 同档 roundtrip: err=%v len=%d", v, err, len(got))
		}
	}
	// 跨档 fail-closed：任一堆用另一档派生 key 解密必失败。
	for a, blob := range blobs {
		for _, b := range tiers {
			if b == a {
				continue
			}
			if _, err := openBlock(keys[b], blob); err == nil {
				t.Errorf("档 %d blob 用档 %d key 解密应失败（跨档 fail-closed）", a, b)
			}
		}
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
	for missing := range blocks {
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

// TestParseBlock_LengthHeaderExactMatch（M2 收紧回归）：长度头必须与文件大小**精确相等**
// （自产生 blob 无尾部噪音）——尾部多字节/长度不符一律 fail-closed，不再宽容 `>len-ctOff`。
func TestParseBlock_LengthHeaderExactMatch(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x77}, KeyLen)
	salt := bytes.Repeat([]byte{0x88}, SaltLen)
	blob, err := sealBlock(key, salt, []byte("hello secret"))
	if err != nil {
		t.Fatalf("sealBlock: %v", err)
	}
	// 正常解析。
	if _, _, ct, err := parseBlock(blob); err != nil {
		t.Fatalf("parseBlock(正常): %v", err)
	} else if string(ct) != "hello secret" && len(ct) != len("hello secret")+16 {
		t.Errorf("ct 长度异常: %d", len(ct))
	}
	// 尾部多 1 字节 → fail-closed（原宽容式 `ctLen > len(blob)-ctOff` 会接受）。
	noisy := append(append([]byte(nil), blob...), 0x00)
	if _, _, _, err := parseBlock(noisy); err == nil {
		t.Error("长度头与文件大小不精确相等应失败（尾部噪音）")
	}
	// 截断 1 字节 → fail-closed。
	trunc := blob[:len(blob)-1]
	if _, _, _, err := parseBlock(trunc); err == nil {
		t.Error("截断 blob 应失败")
	}
}

// TestRegisterAlgorithm_DuplicateNamePanic（M10 补充）：同 Name 不同版本注册必须
// panic——parseAlgorithm 按 Name 匹配依赖 map 迭代，若多名同 Name 会非确定序命中
// （Name 是算法标识，应全局唯一）。测试注册的临时版本须在清理时从共享注册表移除，
// 避免污染其它用例（如 TestDeriveKey_UnknownVersionFails 依赖 99 未注册）。
//
// 串行（不并行）：共享算法注册表按设计「装配期填充、运行期只读」，本测试是少数
// 运行期写注册表的用例，须与并行读用例隔离，否则 -race 报数据竞争。
func TestRegisterAlgorithm_DuplicateNamePanic(t *testing.T) {
	// sproxy:serial: 写共享算法注册表（其它用例并行只读），隔离避免数据竞争。
	const dupName = "shardseal/dup-name-for-test"
	const tmpVersion AlgoVersion = 98 // 用 98 而非 99，避免与 TestDeriveKey_UnknownVersionFails 冲突
	// 清理：无论是否 panic，都从共享注册表移除临时版本。
	t.Cleanup(func() { delete(registry, tmpVersion) })
	defer func() {
		if r := recover(); r == nil {
			t.Error("同 Name 注册应 panic（算法标识必须全局唯一）")
		}
	}()
	// 注册一个唯一版本（不与已注册的 v1 冲突），同 Name 再注册另一版本 → panic。
	RegisterAlgorithm(Algorithm{Version: tmpVersion, Name: dupName})
	RegisterAlgorithm(Algorithm{Version: AlgoVersion(98) + 1, Name: dupName})
}
