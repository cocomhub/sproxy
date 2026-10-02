// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPolicy 返回一个可用于测试的小块策略（默认 1MB 对 2KB 文件不适用）。
func testPolicy() BlockPolicy {
	return BlockPolicy{Mode: "random", Min: 64, Max: 256}
}

// writeTestFile 写一个确定的字节模式文件到临时目录，返回路径与期望内容。
func writeTestFile(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "video.mp4")
	var data []byte
	for i := range 5000 { // 5000B，切开多块（min=64）
		data = append(data, byte(i%251))
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	return p, data
}

func TestEncryptShards_DecryptRoundtrip(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	outDir := t.TempDir()
	secret := []byte("super-secret-32-bytes")

	res, err := EncryptShards(src, outDir, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if res == nil {
		t.Fatal("EncryptShards 返回 nil")
	}
	if len(res.ChunkNames) < 2 {
		t.Fatalf("期望 ≥2 个分块，got %d", len(res.ChunkNames))
	}
	if res.Meta == nil {
		t.Fatal("Meta 为 nil")
	}
	// 分块与 meta 文件确实落到 outDir。
	for _, cn := range res.ChunkNames {
		if _, serr := os.Stat(filepath.Join(outDir, cn)); serr != nil {
			t.Errorf("分块 %q 不存在: %v", cn, serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(outDir, res.MetaName)); serr != nil {
		t.Errorf("meta 文件 %q 不存在: %v", res.MetaName, serr)
	}

	dst := filepath.Join(t.TempDir(), "restored.mp4")
	if derr := DecryptFile(res.Meta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读还原文件失败: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("还原内容不一致：len(got)=%d len(want)=%d", len(got), len(want))
	}
}

func TestDecryptFile_WrongSecretFails(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	res, err := EncryptShards(src, outDir, []byte("right-secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "x.bin")
	if err := DecryptFile(res.Meta, outDir, dst, []byte("wrong-secret")); err == nil {
		t.Fatal("期望错误密钥解密失败，却成功")
	}
}

// TestDecryptChunkStandalone 验证 blob 自描述独立解密：EncryptShards 产分块后，取
// 第一个分块 blob，仅凭 secret + blob 独立还原该块明文（不依赖 meta 的 chunks
// 索引），且与 meta 记录的块内容一致；错误密钥 fail-closed。
func TestDecryptChunkStandalone(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	outDir := t.TempDir()
	secret := []byte("super-secret-32-bytes")

	res, err := EncryptShards(src, outDir, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if len(res.ChunkNames) == 0 || len(res.Meta.Chunks) == 0 {
		t.Fatalf("期望至少一个分块（chunks=%d meta.chunks=%d）", len(res.ChunkNames), len(res.Meta.Chunks))
	}
	// 取第一个分块 blob，独立解密 == 该块原内容（用 meta 的 Offset/OrigSize 定位）。
	first := res.ChunkNames[0]
	blob, err := os.ReadFile(filepath.Join(outDir, first))
	if err != nil {
		t.Fatalf("读第一个分块 %q 失败: %v", first, err)
	}
	ci := res.Meta.Chunks[0]
	wantPlain := want[ci.Offset : ci.Offset+ci.OrigSize]
	got, err := DecryptChunkStandalone(secret, blob)
	if err != nil {
		t.Fatalf("DecryptChunkStandalone: %v", err)
	}
	if !bytes.Equal(got, wantPlain) {
		t.Fatalf("独立解密内容不一致：len(got)=%d len(want)=%d", len(got), len(wantPlain))
	}
	// 错误密钥 fail-closed。
	if _, err := DecryptChunkStandalone([]byte("wrong-secret"), blob); err == nil {
		t.Fatal("期望错误密钥独立解密失败，却成功")
	}
}

// TestDecryptFile_IntegritySHA256Mismatch 验证 DecryptFile 做全量 SHA-256 完整性校验
// （审查 I-X：只按等长逐块比对，等长交换/重排分块会静默产出错内容）。构造 meta 的
// original.sha256 与实际内容不符 → 解密应失败并删除残file。
func TestDecryptFile_IntegritySHA256Mismatch(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	res, err := EncryptShards(src, outDir, []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	// 篡改 meta 声明的整文件 SHA-256（保留等长，只改首字节）。
	stale := res.Meta.Original.SHA256
	res.Meta.Original.SHA256 = "ffffffffffffffff" + stale[16:]
	dst := filepath.Join(t.TempDir(), "restored.mp4")
	err = DecryptFile(res.Meta, outDir, dst, []byte("secret"))
	if err == nil {
		t.Fatal("meta 声明 sha256 与真实内容不符时应解密失败，却成功")
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Error("完整性校验失败后不应残留还原文件")
	}
}

// TestDecryptFile_InvalidMetaSalt 验证 meta.Salt 非法/缺失时 fail-closed
// （Sonar S5344 整文件派生一次依赖 meta.Salt 解码，非法即拒绝）。
func TestDecryptFile_InvalidMetaSalt(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	res, err := EncryptShards(src, outDir, []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	for name, mutate := range map[string]func(*Meta){
		"空 salt":   func(m *Meta) { m.Salt = "" },
		"非 base64": func(m *Meta) { m.Salt = "###not-base64###" },
		"长度错误":     func(m *Meta) { m.Salt = base64.StdEncoding.EncodeToString(make([]byte, 8)) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			meta := *res.Meta
			mutate(&meta)
			if err := DecryptFile(&meta, outDir, filepath.Join(t.TempDir(), "x.bin"), []byte("secret")); err == nil {
				t.Fatal("meta.Salt 非法时应解密失败，却成功")
			}
		})
	}
}

// TestDecryptFile_ChunkSaltMismatch 验证块内 salt 与 meta 声明不一致时 fail-closed
// （decryptBlock 逐块校验，防块被替换/错位）。
func TestDecryptFile_ChunkSaltMismatch(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	res, err := EncryptShards(src, outDir, []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if len(res.ChunkNames) == 0 {
		t.Fatal("期望有分块")
	}
	// 替换首个分块的 salt 段（统一格式 [R][4B 密文长][salt][nonce][ct+tag] 中 salt 位于
	// RandPrefixLen+4 起），保留密文其余部分 → salt 校验应拒绝。注：绝不能篡改 R 段
	// （前 128B 仅混淆、不校验，任务 2 起篡改 R 不影响解密）。
	cn := res.ChunkNames[0]
	blob, rerr := os.ReadFile(filepath.Join(outDir, cn))
	if rerr != nil {
		t.Fatalf("读分块: %v", rerr)
	}
	if len(blob) < saltOff+SaltLen {
		t.Fatalf("分块过短无法定位 salt 段（len=%d）", len(blob))
	}
	tampered := make([]byte, len(blob))
	copy(tampered, blob)
	for i := saltOff; i < saltOff+SaltLen; i++ {
		tampered[i] ^= 0xFF
	}
	if werr := os.WriteFile(filepath.Join(outDir, cn), tampered, 0o600); werr != nil {
		t.Fatalf("写篡改分块: %v", werr)
	}
	if err := DecryptFile(res.Meta, outDir, filepath.Join(t.TempDir(), "x.bin"), []byte("secret")); err == nil {
		t.Fatal("块内 salt 与 meta 不一致时应解密失败，却成功")
	}
}

func TestMeta_HasFullStat(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	res, err := EncryptShards(src, t.TempDir(), []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	o := res.Meta.Original
	if o.Name != "video.mp4" {
		t.Errorf("original.name=%q", o.Name)
	}
	if o.Size != st.Size() {
		t.Errorf("original.size=%d want %d", o.Size, st.Size())
	}
	if res.Meta.Algorithm != AlgorithmName {
		t.Errorf("algorithm=%q", res.Meta.Algorithm)
	}
	if res.Meta.KDF != "scrypt" {
		t.Errorf("kdf=%q", res.Meta.KDF)
	}
	if len(res.ChunkNames) != len(res.Meta.Chunks) {
		t.Errorf("chunk 数与 meta.chunks 数不一致：%d vs %d", len(res.ChunkNames), len(res.Meta.Chunks))
	}
	sum := int64(0)
	for i, cn := range res.Meta.Chunks {
		if cn.Index != i {
			t.Errorf("chunks[%d].index=%d", i, cn.Index)
		}
		if len(cn.OrigSHA256) != 16 {
			t.Errorf("orig_sha256 长度=%d，应为 16", len(cn.OrigSHA256))
		}
		if len(cn.EncSHA256) != 16 {
			t.Errorf("enc_sha256 长度=%d，应为 16", len(cn.EncSHA256))
		}
		if cn.OrigSize <= 0 {
			t.Errorf("chunks[%d].orig_size=%d", i, cn.OrigSize)
		}
		// Offset 应等于前 i 块 OrigSize 累计（连续覆盖 [0, 文件总大小)；i=0 时 sum=0）。
		if cn.Offset != sum {
			t.Errorf("chunks[%d].offset=%d，应为前 %d 块累计 %d", i, cn.Offset, i, sum)
		}
		sum += cn.OrigSize
	}
	if sum != st.Size() {
		t.Errorf("分块原始大小合计=%d want %d", sum, st.Size())
	}
	// 末块 offset+orig_size 应恰好等于文件大小（随机访问区间右端点）。
	if last := res.Meta.Chunks[len(res.Meta.Chunks)-1]; last.Offset+last.OrigSize != st.Size() {
		t.Errorf("末块 offset+orig_size=%d，应为文件大小 %d", last.Offset+last.OrigSize, st.Size())
	}
	// salt 应 base64 可解码。
	if _, err := base64.StdEncoding.DecodeString(res.Meta.Salt); err != nil {
		t.Errorf("salt 非 base64: %v", err)
	}
}

func TestEncryptShards_NamingConvention(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	res, err := EncryptShards(src, t.TempDir(), []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	total := sha256.Sum256(want)
	totalHex := to16Hex(total[:])
	for _, cn := range res.ChunkNames {
		// 三段 16hex，中段 = 原始总校验和前 16。
		if !strings.Contains(cn, totalHex) {
			t.Errorf("分块名 %q 缺原始总校验和 %q（命名规则三段中间段）", cn, totalHex)
		}
		if strings.ContainsAny(cn, "-_") {
			t.Errorf("分块名 %q 不应含 -/_（仅 meta 可含）", cn)
		}
	}
	// meta 名必含 - 或 _（可识别标记）。
	if !strings.ContainsAny(res.MetaName, "-_") {
		t.Errorf("meta 名 %q 应含 - 或 _", res.MetaName)
	}
	// meta 名也含原始总 16 hex。
	if !strings.Contains(res.MetaName, totalHex) {
		t.Errorf("meta 名 %q 应含原始总校验和段 %q", res.MetaName, totalHex)
	}
}

// TestEncryptShards_MetaEncryptedOnDisk：meta 加密为密文（非明文 JSON），且带 R 首部
// + 长度头线性一致；res.MetaBlob 直接返回最终 blob（与落盘一致），meta 名三段真实锚定
// 最终 blob（末段 = hash16(MetaBlob)）。
func TestEncryptShards_MetaEncryptedOnDisk(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	res, err := EncryptShards(src, outDir, []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if len(res.MetaBlob) == 0 {
		t.Fatal("MetaBlob 不应为空")
	}
	// res.MetaBlob 与落盘文件一致（不上传中间 blob_A）。
	onDisk, err := os.ReadFile(filepath.Join(outDir, res.MetaName))
	if err != nil {
		t.Fatalf("读 meta 落盘: %v", err)
	}
	if !bytes.Equal(res.MetaBlob, onDisk) {
		t.Error("res.MetaBlob 与落盘 meta 文件应一致")
	}
	if bytes.Contains(res.MetaBlob, []byte(`"version"`)) {
		t.Error("meta 落盘不应是明文 JSON")
	}
	// 长度断言：padTarget=0 无 padding，meta 明文 = [4B jsonLen][metaJSON]，密文长 = 明文+16。
	metaJSON, _ := json.Marshal(res.Meta)
	want := RandPrefixLen + hdrLen + SaltLen + NonceLen + (4 + len(metaJSON)) + 16
	if len(res.MetaBlob) != want {
		t.Errorf("meta 落盘长度 %d，应为 %d（R+8B长度头+salt+nonce+jsonLen+JSON+tag）", len(res.MetaBlob), want)
	}
	// meta 名末段 = hash16(MetaBlob)（名字锚定最终 blob），首段非全零（真实哈希）。
	encHex, herr := hash16(res.MetaBlob)
	if herr != nil {
		t.Fatalf("hash16: %v", herr)
	}
	if got := res.MetaName[len(res.MetaName)-16:]; got != encHex {
		t.Errorf("meta 名末段 %q 应为最终 blob 哈希 %q", got, encHex)
	}
	if strings.HasPrefix(res.MetaName, "0000000000000000") {
		t.Errorf("meta 名首段应为真实哈希，当前是占位：%q", res.MetaName)
	}
}

func TestMetaJSONRoundtrip(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	res, err := EncryptShards(src, t.TempDir(), []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	data, err := json.Marshal(res.Meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	var back Meta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if back.Version != res.Meta.Version || back.Algorithm != res.Meta.Algorithm ||
		back.Original.Name != res.Meta.Original.Name || back.Original.Size != res.Meta.Original.Size {
		t.Errorf("roundtrip 不一致: %+v vs %+v", back, res.Meta)
	}
	if len(back.Chunks) != len(res.Meta.Chunks) {
		t.Errorf("chunks 数量 roundtrip 不一致")
	}
}

// TestDecrypt_ByMetaVersion 验证 meta.algo_version 驱动解密选派生：algo_version=1 解密
// 成功（EncryptShards 已写 v1 并记录于 meta）；未知版本 fail-closed（validateMeta 拒绝，
// 不进入派生/解密）。
func TestDecrypt_ByMetaVersion(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	secret := []byte("super-secret-32-bytes")
	res, err := EncryptShards(src, outDir, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if res.Meta.AlgoVersion != AlgoV1GCM {
		t.Fatalf("meta.algo_version=%d，应为 %d（EncryptShards 记 v1）", res.Meta.AlgoVersion, AlgoV1GCM)
	}

	// meta.algo_version=1 解密成功。
	ok := *res.Meta
	if derr := DecryptFile(&ok, outDir, filepath.Join(t.TempDir(), "r.bin"), secret); derr != nil {
		t.Fatalf("algo_version=1 解密失败: %v", derr)
	}

	// 未知版本 fail-closed（validateMeta 拒之门外）。
	unknown := *res.Meta
	unknown.AlgoVersion = AlgoVersion(99)
	if derr := DecryptFile(&unknown, outDir, filepath.Join(t.TempDir(), "x.bin"), secret); derr == nil {
		t.Fatal("未知算法版本应解密失败（fail-closed），却成功")
	}
}

// TestDecryptChunkStandalone_AllVersions：独立解密按注册表试各算法版本派生——仅凭
// secret+分块 blob 独立解出块明文；meta blob 亦经 DecryptMetaStandalone 独立破解
// （版本在密文内，试派生定位；secretdata 卷重启加载用）。错误密钥 fail-closed。
func TestDecryptChunkStandalone_AllVersions(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	outDir := t.TempDir()
	secret := []byte("super-secret-32-bytes")
	res, err := EncryptShards(src, outDir, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if len(res.ChunkNames) == 0 || len(res.Meta.Chunks) == 0 {
		t.Fatal("期望有分块")
	}

	first := res.ChunkNames[0]
	blob, err := os.ReadFile(filepath.Join(outDir, first))
	if err != nil {
		t.Fatalf("读第一个分块 %q 失败: %v", first, err)
	}
	ci := res.Meta.Chunks[0]
	wantPlain := want[ci.Offset : ci.Offset+ci.OrigSize]
	got, err := DecryptChunkStandalone(secret, blob)
	if err != nil {
		t.Fatalf("DecryptChunkStandalone(按注册表试派生): %v", err)
	}
	if !bytes.Equal(got, wantPlain) {
		t.Fatalf("独立解内容不一致: len(got)=%d len(want)=%d", len(got), len(wantPlain))
	}

	// meta blob 独立解密（试派生）还原有效 meta JSON——secretdata.decryptBlob 依赖此路径。
	metaRaw, merr := DecryptMetaStandalone(secret, res.MetaBlob)
	if merr != nil {
		t.Fatalf("DecryptMetaStandalone: %v", merr)
	}
	var m Meta
	if uerr := json.Unmarshal(metaRaw, &m); uerr != nil || m.Original.Name == "" {
		t.Errorf("meta 独立解未还原有效 JSON meta（unmarshal=%v name=%q）", uerr, m.Original.Name)
	}

	// 错误密钥 fail-closed（分块独立解尝遍注册表仍失败）。
	if _, err := DecryptChunkStandalone([]byte("wrong-secret"), blob); err == nil {
		t.Fatal("错误密钥独立解应失败，却成功")
	}
}

// TestValidateMeta_TmpV2RegisteredAlg 临时向全局注册表登记一个 v2 算法，断言其 meta
// 通过 validateMeta——证明算法校验**经注册表**（注册即生效），而非硬编码
// AlgorithmName（若硬编码，v2 名字串会「自己写自己读不过」）。
// sproxy:serial: 临时登记/清理全局注册表 v2，须非并行避免与并行测试竞态
func TestValidateMetaTmpV2RegisteredAlg(t *testing.T) {
	// sproxy:serial: 全局注册表临时登记 v2，完事 defer 删除；非并行运行
	RegisterAlgorithm(Algorithm{
		Version:   AlgoVersion(2),
		Name:      "shardseal/v2-test",
		KDFDomain: "shardseal/v2-test",
		Encrypt:   sealBlock,
		Decrypt:   decryptBlock,
	})
	defer delete(registry, AlgoVersion(2))

	tmp := &Meta{
		Version:     metaVersion,
		Algorithm:   "shardseal/v2-test",
		AlgoVersion: AlgoVersion(2),
		KDF:         "scrypt",
		Original:    OriginalInfo{Name: "x.bin", Size: 3},
		Chunks: []ChunkInfo{
			{FileName: "a1b2", OrigSize: 3, Blocklets: []BlockletInfo{{Offset: 0, Size: 3, EncSize: 16}}},
		},
	}
	if err := validateMeta(tmp); err != nil {
		t.Fatalf("已注册 v2 应通过 validateMeta（经注册表），实为: %v", err)
	}

	// 名字↔版本不一致 fail-closed（名 v2-test 却写版本 v1）。
	mismatch := *tmp
	mismatch.AlgoVersion = AlgoV1GCM
	if err := validateMeta(&mismatch); err == nil {
		t.Error("算法名与版本不一致应 fail-closed，却通过")
	}

	// deriveKey 亦按版本域派生成功（注册 v2 后即可用）。
	if _, err := deriveKey([]byte("s"), make([]byte, SaltLen), AlgoVersion(2)); err != nil {
		t.Errorf("已注册 v2 派生应成功: %v", err)
	}
}

// TestValidateMeta_UnregisteredNameFails：未注册算法名 fail-closed。
func TestValidateMeta_UnregisteredNameFails(t *testing.T) {
	t.Parallel()
	m := &Meta{
		Version:     metaVersion,
		Algorithm:   "ghost/aes-256-cbc",
		AlgoVersion: AlgoV1GCM,
		Original:    OriginalInfo{Name: "x", Size: 1},
		Chunks:      []ChunkInfo{{FileName: "y", OrigSize: 1}},
	}
	if err := validateMeta(m); err == nil {
		t.Error("未注册算法名应 fail-closed，却通过")
	}
}

// TestBlocklet_OffsetsCoverFile 验证 EncryptShards 产出的 meta.blocklets 连续覆盖各块
// [Offset, Offset+OrigSize)：首 blocklet 偏移 = 块偏移、尺寸和 = OrigSize、EncSize>0。
func TestBlocklet_OffsetsCoverFile(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	res, err := EncryptShards(src, t.TempDir(), []byte("secret"), testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	for i, ci := range res.Meta.Chunks {
		if len(ci.Blocklets) == 0 {
			t.Errorf("分块 %d 无 blocklet 索引", i)
			continue
		}
		var cur = ci.Offset
		for j, bl := range ci.Blocklets {
			if bl.Offset != cur {
				t.Errorf("分块[%d].blocklet[%d].offset=%d，应为连续 %d", i, j, bl.Offset, cur)
			}
			if bl.Size <= 0 || bl.EncSize <= 0 || bl.EncOffset <= 0 {
				t.Errorf("分块[%d].blocklet[%d] size/enc_size/enc_offset 非法：%+v", i, j, bl)
			}
			if !bl.Used {
				t.Errorf("分块[%d].blocklet[%d] used=false，当前固定规划产出应全为已用", i, j)
			}
			if bl.Type != byte(BlockletTypeData) {
				t.Errorf("分块[%d].blocklet[%d] type=0x%02x，应为 Data(0x01)", i, j, bl.Type)
			}
			cur += bl.Size
		}
		if cur != ci.Offset+ci.OrigSize {
			t.Errorf("分块[%d] blocklet 覆盖 [%d,%d)，应 [%d,%d)", i, ci.Offset, cur, ci.Offset, ci.Offset+ci.OrigSize)
		}
		// blocklet 密文段大小应为 nonce + 明文 + GCM tag（随机访问定位段长用）。
		for j, bl := range ci.Blocklets {
			if want := blockletEncSize(bl.Size); bl.EncSize != want {
				t.Errorf("分块[%d].blocklet[%d].enc_size=%d，应为 %d", i, j, bl.EncSize, want)
			}
		}
	}
}

// TestDecryptFile_BlockletFull 全量还原仍正确：多块多 blocklet 文件 EncryptShards →
// DecryptFile → 整文件内容 + SHA-256 校验通过（fail-closed）。
func TestDecryptFile_BlockletFull(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	outDir := t.TempDir()
	// 小块策略 + 小 blocklet → 多块、多块多 blocklet。
	policy := BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMin: 32, BlockletMax: 64}
	res, err := EncryptShards(src, outDir, []byte("secret"), policy, 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	multi := false
	for _, ci := range res.Meta.Chunks {
		if len(ci.Blocklets) > 1 {
			multi = true
		}
	}
	if !multi {
		t.Fatalf("测试前提不成立：期望至少一块含多个 blocklet（chunks=%d）", len(res.Meta.Chunks))
	}
	dst := filepath.Join(t.TempDir(), "restored.mp4")
	if derr := DecryptFile(res.Meta, outDir, dst, []byte("secret")); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读还原文件失败: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("还原内容不一致：len(got)=%d len(want)=%d", len(got), len(want))
	}
	// 全量 SHA-256 完整性（writeDecryptedChunks 累加，DecryptFile fail-closed）。
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Error("整文件 SHA-256 校验应通过")
	}
}

// TestEncryptBlocklets_PaddingAndExtraFormat 锁定格式预留：padding blocklet（type 0x02）
// 用随机字节填充（密文内、GCM 认证），不属于文件逻辑内容——decryptBlock 全量解只拼数据
// 段、decryptBlockletAt 目标落在 padding 段 fail-closed；meta 驱动的 DecryptBlockletAt 仍
// 可独立解出 padding 填充；padding 与已用区间重叠 fail-closed。
func TestEncryptBlocklets_PaddingAndExtraFormat(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x44}, KeyLen)
	salt := bytes.Repeat([]byte{0x55}, SaltLen)
	// 已用 blocklet 覆盖 [0,16)，padding blocklet 预留 [16,32)（打包替换：多余段标记空闲）。
	blocklets := []Blocklet{{Offset: 0, Size: 16}, {Offset: 16, Size: 16, Padding: true}}
	data := make([]byte, 16)
	for i := range data {
		data[i] = byte(i)
	}
	blob, entries, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("索引条目数=%d，应为 2", len(entries))
	}
	if entries[0].Type != BlockletTypeData || entries[1].Type != BlockletTypePadding {
		t.Errorf("索引类型不符：%+v", entries)
	}
	// 全量解密只拼数据段：16B 原文（padding 跳过，不进入逻辑内容）。
	plain, err := decryptBlock(key, salt, blob)
	if err != nil {
		t.Fatalf("decryptBlock: %v", err)
	}
	if len(plain) != 16 || !bytes.Equal(plain, data) {
		t.Errorf("decryptBlock 应只返回已用段原文（len=%d）", len(plain))
	}
	// 目标落在 padding 段 → fail-closed（非数据段不可作为文件内容读取）。
	if _, _, err := decryptBlockletAt(key, salt, blob, 16); err == nil {
		t.Error("目标落在 padding 段应 fail-closed")
	}
	// meta 驱动的 DecryptBlockletAt（type=Padding）仍可独立解出随机填充（GCM 认证）。
	padInfo := BlockletInfo{
		Offset: 16, Size: 16,
		EncOffset: int64(entries[1].EncOffset), EncSize: entries[1].EncSize,
		Type: byte(BlockletTypePadding),
	}
	pad, _, derr := DecryptBlockletAt(key, salt, blob, 0, padInfo)
	if derr != nil {
		t.Fatalf("DecryptBlockletAt(padding): %v", derr)
	}
	if len(pad) != 16 {
		t.Errorf("padding 段解出长度 %d，应为 16", len(pad))
	}
	// padding 与已用区间重叠 → fail-closed。
	if _, _, err := encryptBlocklets(key, salt, []Blocklet{{Offset: 8, Size: 16, Padding: true}}, data, AlgoV1GCM); err == nil {
		t.Error("padding 与已用区间重叠应 fail-closed")
	}
}

// TestBlob_NoPlaintextSegmentHeaders 验证段边界全部不明文：type/off/len 只作 GCM AAD，
// blob 明文（含 R、8B 总长、salt 之后的随机字节流）不包含密封时使用的任一 AAD 序列——
// 观察者无法切分、无类型分布、无偏移布局。
func TestBlob_NoPlaintextSegmentHeaders(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x66}, KeyLen)
	salt := bytes.Repeat([]byte{0x77}, SaltLen)
	blocklets := []Blocklet{{Offset: 100, Size: 16}, {Offset: 116, Size: 16}, {Offset: 132, Size: 8}}
	data := make([]byte, 40)
	for i := range data {
		data[i] = byte(i * 3)
	}
	blob, _, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	// 数据段 AAD（type/off/len）不得作为 blob 明文子序列出现。
	for _, bl := range blocklets {
		aad := encodeBlockletAAD(blockletType(bl), bl.Offset-blocklets[0].Offset, bl.Size)
		if bytes.Contains(blob, aad) {
			t.Errorf("blob 明文含 AAD 段头 %x（type/off/len 应只作 GCM AAD，不落盘明文）", aad)
		}
	}
	// boot/index 的 AAD 同样不得明文出现（0x0F/0x10 类型字节只在 AAD 中）。
	if bytes.Contains(blob, encodeBlockletAAD(BlockletTypeBoot, 0, bootPlainLen)) {
		t.Error("boot 段头（type 0x0F）不应明文出现")
	}
	if bytes.Contains(blob, []byte{byte(BlockletTypeIndex), 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Error("index 段头（type 0x10）不应明文出现")
	}
}

// TestDecrypt_SequentialNoMeta 验证无 meta 全量还原：仅凭 secret+块 blob，经 boot→index
// 顺序解出全部数据段（DecryptChunkStandalone）。错误密钥 fail-closed。
func TestDecrypt_SequentialNoMeta(t *testing.T) {
	t.Parallel()
	secret := []byte("super-secret-32-bytes")
	salt := bytes.Repeat([]byte{0x66}, SaltLen)
	key, err := deriveKey(secret, salt, AlgoV1GCM)
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	p := &FixedBlockletPlanner{Min: 4, Max: 16}
	blocklets, perr := p.PlanBlocklets(nil, 200, 0, 200)
	if perr != nil {
		t.Fatalf("PlanBlocklets: %v", perr)
	}
	data := make([]byte, 200)
	for i := range data {
		data[i] = byte(i % 251)
	}
	blob, _, eerr := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if eerr != nil {
		t.Fatalf("encryptBlocklets: %v", eerr)
	}
	got, derr := DecryptChunkStandalone(secret, blob)
	if derr != nil {
		t.Fatalf("DecryptChunkStandalone: %v", derr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("无 meta 全量还原内容不一致：len(got)=%d", len(got))
	}
	if _, derr := DecryptChunkStandalone([]byte("wrong-secret"), blob); derr == nil {
		t.Error("错误密钥应 fail-closed")
	}
}

// TestIndexBlock_RandomAccessNoMeta 验证无 meta 随机访问：仅凭 secret+块 blob，经
// boot→index 定位目标数据段并只解该段（视频关键帧随机访问基础）。
func TestIndexBlock_RandomAccessNoMeta(t *testing.T) {
	t.Parallel()
	secret := []byte("super-secret-32-bytes")
	salt := bytes.Repeat([]byte{0x77}, SaltLen)
	key, err := deriveKey(secret, salt, AlgoV1GCM)
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	blocklets := []Blocklet{{Offset: 0, Size: 32}, {Offset: 32, Size: 48}, {Offset: 80, Size: 16}}
	data := make([]byte, 96)
	for i := range data {
		data[i] = byte(i)
	}
	blob, _, eerr := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if eerr != nil {
		t.Fatalf("encryptBlocklets: %v", eerr)
	}
	// 目标 40 位于第二个 blocklet [32,80)。
	const target = int64(40)
	got, derr := DecryptBlockletStandalone(secret, blob, target)
	if derr != nil {
		t.Fatalf("DecryptBlockletStandalone: %v", derr)
	}
	want := data[32:80]
	if !bytes.Equal(got, want) {
		t.Errorf("无 meta 随机访问只应返回目标 blocklet：len(got)=%d，应为 %d", len(got), len(want))
	}
	// 越界目标 → fail-closed。
	if _, derr := DecryptBlockletStandalone(secret, blob, 500); derr == nil {
		t.Error("越界目标应 fail-closed")
	}
}

// TestBlockletType_Table 验证类型表：Data/Padding/Extra/Boot/Index 各段按 type 正确解密
// 分派；未知 type 索引条目 fail-closed。
func TestBlockletType_Table(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x88}, KeyLen)
	salt := bytes.Repeat([]byte{0x99}, SaltLen)
	// Data + Extra + Padding 混合段：索引记录各自 type，解密路径按 type 分派。
	blocklets := []Blocklet{
		{Offset: 0, Size: 16},                           // Data
		{Offset: 16, Size: 16, Type: BlockletTypeExtra}, // Extra（预留槽位）
		{Offset: 32, Size: 16, Padding: true},           // Padding
	}
	data := make([]byte, 16)
	for i := range data {
		data[i] = byte(i)
	}
	blob, entries, err := encryptBlocklets(key, salt, blocklets, data, AlgoV1GCM)
	if err != nil {
		t.Fatalf("encryptBlocklets: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries=%d，应为 3", len(entries))
	}
	wantTypes := []BlockletType{BlockletTypeData, BlockletTypeExtra, BlockletTypePadding}
	for i, e := range entries {
		if e.Type != wantTypes[i] {
			t.Errorf("entries[%d].type=0x%02x，应为 0x%02x", i, byte(e.Type), byte(wantTypes[i]))
		}
	}
	// 全量还原只拼 Data 段（Extra/Padding 跳过）。
	plain, err := decryptBlock(key, salt, blob)
	if err != nil || !bytes.Equal(plain, data) {
		t.Errorf("decryptBlock 应只返回 Data 段：err=%v len=%d", err, len(plain))
	}
	// Extra/Padding 段 meta 驱动可独立解出（各自类型 AAD 认证）。
	for i, e := range entries {
		if e.Type == BlockletTypeData {
			continue
		}
		info := BlockletInfo{Offset: blocklets[i].Offset, Size: blocklets[i].Size,
			EncOffset: int64(e.EncOffset), EncSize: e.EncSize, Type: byte(e.Type)}
		seg, _, derr := DecryptBlockletAt(key, salt, blob, 0, info)
		if derr != nil {
			t.Errorf("type 0x%02x 段独立解失败: %v", byte(e.Type), derr)
		}
		if int64(len(seg)) != blocklets[i].Size {
			t.Errorf("type 0x%02x 段长 %d，应为 %d", byte(e.Type), len(seg), blocklets[i].Size)
		}
	}
	// 未知 type（0x11+ 预留）fail-closed。
	if err := validateIndexEntries([]BlobIndexEntry{
		{Type: BlockletTypeReserved, Off: 0, Len: 1, EncOffset: blListOff, EncSize: NonceLen + 1 + 16},
	}, blob); err == nil {
		t.Error("未知 type 索引条目应 fail-closed")
	}
}

// TestReplaceBlocklet_OthersDirectlyUsable 锁定「段隔离 + 拼接」硬保证（用户 21:30 关键
// 架构验证）：多块文件加密后替换其中一个块的内容（重新加密为新块 blob），断言——
//  1. 其它未变块的 blob 不重新加密、原样保留（meta 直接复用其 FileName，还原时直接拼接）；
//  2. meta 布局更新（被替换块换名 + 整文件 SHA256 更新）；
//  3. 用原未变块 blob + 新块 blob + 更新后 meta 还原 = 替换后内容（SHA256 匹配）。
//
// 本质验证：blob 之间无交叉依赖（独立 GCM + 内嵌 salt/offset），拼接只依赖 meta 索引。
// 若加密路径块/blocklet 有隐含共享状态（nonce/salt），此测试必红。
func TestReplaceBlocklet_OthersDirectlyUsable(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	outDir := t.TempDir()
	secret := []byte("super-secret-32-bytes")
	policy := BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMin: 32, BlockletMax: 64}
	res, err := EncryptShards(src, outDir, secret, policy, 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if len(res.Meta.Chunks) < 2 {
		t.Fatalf("测试前提：期望 ≥2 块（got %d）", len(res.Meta.Chunks))
	}

	// 选中间块替换（保留首末块原样）。
	replaceIdx := 1
	ci := res.Meta.Chunks[replaceIdx]
	origChunk := want[ci.Offset : ci.Offset+ci.OrigSize]
	newChunk := make([]byte, len(origChunk))
	for i := range newChunk {
		newChunk[i] = origChunk[i] ^ 0xFF
	}
	// 用同一文件级 salt/key 重新加密被替换块（块独立：不依赖其它块状态）。
	salt, serr := decodeSalt(res.Meta)
	if serr != nil {
		t.Fatalf("decodeSalt: %v", serr)
	}
	key, kerr := deriveKey(secret, salt, res.Meta.AlgoVersion)
	if kerr != nil {
		t.Fatalf("deriveKey: %v", kerr)
	}
	blp := &FixedBlockletPlanner{Min: 32, Max: 64}
	blocklets, perr := blp.PlanBlocklets(nil, int64(len(want)), ci.Offset, ci.OrigSize)
	if perr != nil {
		t.Fatalf("PlanBlocklets: %v", perr)
	}
	newBlob, entries, eerr := encryptBlocklets(key, salt, blocklets, newChunk, res.Meta.AlgoVersion)
	if eerr != nil {
		t.Fatalf("encryptBlocklets: %v", eerr)
	}
	totalHex, _ := hash16(want)
	origHex, _ := hash16(newChunk)
	encHex, _ := hash16(newBlob)
	newName := ChunkName(origHex, totalHex, encHex)
	if werr := os.WriteFile(filepath.Join(outDir, newName), newBlob, 0o600); werr != nil {
		t.Fatalf("写新分块: %v", werr)
	}

	// 更新 meta：被替换块换名 + 换 stat；整文件 SHA256 同步为替换后内容。
	newMeta := *res.Meta
	newMeta.Chunks = append([]ChunkInfo(nil), res.Meta.Chunks...)
	var blInfos []BlockletInfo
	for j, bl := range blocklets {
		e := entries[j]
		seg := newChunk[bl.Offset-ci.Offset : bl.Offset-ci.Offset+bl.Size]
		segHex, _ := hash16(seg)
		blInfos = append(blInfos, BlockletInfo{
			Offset: bl.Offset, Size: bl.Size,
			EncOffset: int64(e.EncOffset), EncSize: e.EncSize,
			OrigSHA256: segHex, Used: true, Type: byte(BlockletTypeData),
		})
	}
	newMeta.Chunks[replaceIdx] = ChunkInfo{
		Index: ci.Index, FileName: newName, Offset: ci.Offset, OrigSize: ci.OrigSize,
		OrigSHA256: origHex, EncSize: int64(len(newBlob)), EncSHA256: encHex, Blocklets: blInfos,
	}
	modData := append([]byte(nil), want...)
	copy(modData[ci.Offset:ci.Offset+ci.OrigSize], newChunk)
	newMeta.Original.SHA256 = sha256Hex64(modData)

	// 断言 1：其它未变块的 blob 未被重新加密——原 FileName 原样保留、原 blob 原样存在。
	for i, c := range res.Meta.Chunks {
		if i == replaceIdx {
			continue
		}
		if newMeta.Chunks[i].FileName != c.FileName {
			t.Errorf("未变块 %d 不应重命名（%q → %q）", i, c.FileName, newMeta.Chunks[i].FileName)
		}
		if _, serr := os.Stat(filepath.Join(outDir, c.FileName)); serr != nil {
			t.Errorf("未变块 %d 原 blob %q 应原样存在: %v", i, c.FileName, serr)
		}
	}
	// 断言 2：meta 布局更新。
	if newMeta.Chunks[replaceIdx].FileName != newName {
		t.Error("被替换块 FileName 未更新")
	}
	if newMeta.Original.SHA256 != sha256Hex64(modData) {
		t.Error("meta 整文件 SHA256 未更新为替换后内容")
	}
	// 断言 3：原未变块 blob + 新块 blob + 更新 meta 还原 = 替换后内容（SHA256 匹配）。
	dst := filepath.Join(t.TempDir(), "replaced.mp4")
	if derr := DecryptFile(&newMeta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("读还原文件失败: %v", rerr)
	}
	if !bytes.Equal(got, modData) {
		t.Fatalf("还原内容 = 替换后内容：len(got)=%d len(want)=%d", len(got), len(modData))
	}
}
