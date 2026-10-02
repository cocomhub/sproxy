// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/crypto/scrypt"
)

// 设计 §4.2：AES-256-GCM + scrypt 派生。
//
//   - 每文件随机盐（32B）→ scrypt(secret, salt) 派生文件密钥（32B）；
//   - 每段随机 nonce（12B）；
//   - meta blob（统一落盘格式）：
//     [R 128B 随机首部][8B 密文长 BE][salt][nonce][ciphertext+GCMtag]。
//     首部/大小/长度头三维度均无 meta/分块特征，无法凭 file/magic/长度区分。
//     R 段仅混淆，不参与校验（篡改不影响解密）；8B 长度头 = 密文长度（len(明文)+16），
//     与文件大小线性一致：文件大小 = R + 8 + 32 + 12 + len(密文)，恒成立。
//   - 分块 blob（块内 blocklet 序列，最终定稿）：
//     [R 128B][8B 密文流总长][salt][boot 引导段][index 索引块][数据/padding/extra 段乱序]。
//     段边界全部不明文（type/off/len 只作 GCM AAD），段间随机 padding 间隙混淆边界；
//     观察者只见随机字节流、不可切分。有 blob+secret 即可经 boot→index 随机访问。
//   - meta 明文额外带 4B jsonLen 前缀与随机 padding（encryptMetaJSON 职责）；padding
//     在密文内、属 GCM 认证范围，解密按 jsonLen 截取真实 JSON。
//
// scrypt 参数（N=131072=2^17, r=8, p=1）：OWASP 交互式登录推荐档位（~100ms 量级），
// 适合低频整文件加密/解密路径；不用于高频流路径（本包按设计只做分块整文件加密）。
// N=65536 曾判定为弱档（Sonar go:S5344），2026-10-02 提升至 2^17。
// 派生为每文件一次（解密路径由 meta.Salt 派生一次后逐块复用，不做逐块派生）。

// scryptN/scryptR/scryptP 是 scrypt 派生参数。
const (
	scryptN = 1 << 17
	scryptR = 8
	scryptP = 1
	// SaltLen 是文件级盐长度（32B）。
	SaltLen = 32
	// NonceLen 是 GCM nonce 长度（12B）。
	NonceLen = 12
	// KeyLen 是派生的文件密钥长度（32B，AES-256）。
	KeyLen = 32
	// RandPrefixLen 是固定长度随机首部（R 段），每文件随机字节——首部无格式指纹
	// （magic/file 不可识别）。分块与 meta 落盘共用。
	RandPrefixLen = 128
)

// AlgoVersion 是加密算法版本（算法域分离：版本经 KDF 派生域混入密钥，blob 明文
// 内不明文存储、避免特征；meta.algo_version 在密文内作权威）。域不同 → 派生 key
// 不同，可自由扩展算法而不破坏既有 blob。
type AlgoVersion byte

const (
	// AlgoV1GCM 是 AES-256-GCM 算法版本（当前唯一实现）。KDF 派生域为显式域标记
	// "shardseal/v1"（功能未上线无旧 blob 需兼容）。
	AlgoV1GCM AlgoVersion = 1
)

// Algorithm 是注册的算法定义：版本 + 标识 + 派生域标记 + 加解密工厂。Encrypt/Decrypt
// 签名先定义为统一形式（未来算法实现用），由各版本注册时提供。
type Algorithm struct {
	Version AlgoVersion
	// Name 是算法标识字符串（即 Meta.Algorithm，如 "shardseal/aes-256-gcm"；装配层
	// Options.Algorithm 按此解析到版本——secretdata 零绑定版本）。
	Name      string
	KDFDomain string // scrypt 派生域标记（混入 secret，域不同 key 不同）
	// Encrypt/Decrypt 工厂（key/salt/blob 均为字节）：本 PR 仅 AES-256-GCM，其余版本
	// 实现扩展时复用同一签名（注册表按版本试派生）。
	Encrypt func(key, salt, plain []byte) ([]byte, error)
	Decrypt func(key, salt, blob []byte) ([]byte, error)
}

// registry 是算法注册表（按版本号索引；装配期填充，运行期只读）。
var registry = map[AlgoVersion]Algorithm{}

// ErrUnknownAlgorithm 是 ResolveAlgorithm 的哨兵错误：算法标识未注册。
var ErrUnknownAlgorithm = errors.New("shardseal: 未知算法")

// RegisterAlgorithm 注册算法（装配期调用）。重复版本 fail-fast panic（杜绝版本
// 显式地覆盖既有注册导致旧 blob 解密视图漂移）；Name 为空同样拒绝。
func RegisterAlgorithm(a Algorithm) {
	if a.Version == 0 {
		panic("shardseal: RegisterAlgorithm 版本 0 非法（版本从 1 起）")
	}
	if a.Name == "" {
		panic(fmt.Sprintf("shardseal: RegisterAlgorithm 算法 %d 标识为空", a.Version))
	}
	if _, ok := registry[a.Version]; ok {
		panic(fmt.Sprintf("shardseal: 算法版本 %d 重复注册", a.Version))
	}
	registry[a.Version] = a
}

// parseAlgorithm 查注册表把算法名解析为已注册版本（未注册返回 false）。
// validateMeta / ResolveAlgorithm 共用——算法校验只经注册表，不硬编码任意名字。
func parseAlgorithm(name string) (AlgoVersion, bool) {
	for _, a := range registry {
		if a.Name == name {
			return a.Version, true
		}
	}
	return 0, false
}

// ResolveAlgorithm 按算法标识字符串（如 "shardseal/aes-256-gcm"）解析已注册版本。
// 未注册/未知算法返回哨兵错误 ErrUnknownAlgorithm（fail-fast 用，不静默回落默认）。
func ResolveAlgorithm(name string) (AlgoVersion, error) {
	if v, ok := parseAlgorithm(name); ok {
		return v, nil
	}
	return 0, fmt.Errorf("%w: %q", ErrUnknownAlgorithm, name)
}

// algorithmName 返回算法版本的注册标识（写进 meta.algorithm）。版本未注册时回落
// AlgorithmName（仅防御性；EncryptShards 内 deriveKey 已先验证 v 注册）。
func algorithmName(v AlgoVersion) string {
	if a, ok := registry[v]; ok {
		return a.Name
	}
	return AlgorithmName
}

// sortedAlgoVersions 返回按版本号升序的已注册算法版本（decrypt 尝遍注册表用；试
// 派生升序 → v1 优先，唯一版本时即单次 scrypt 零额外成本）。
func sortedAlgoVersions() []AlgoVersion {
	vs := make([]AlgoVersion, 0, len(registry))
	for v := range registry {
		vs = append(vs, v)
	}
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && vs[j] < vs[j-1]; j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
	return vs
}

func init() {
	// 装配期注册唯一算法版本 v1（AES-256-GCM）。KDFDomain 为显式域标记
	// "shardseal/v1"（无旧 blob 需兼容——功能未上线；域不同 key 不同）。
	RegisterAlgorithm(Algorithm{
		Version:   AlgoV1GCM,
		Name:      AlgorithmName,
		KDFDomain: "shardseal/v1", // scrypt 派生域标记：混入 secret，版本分离
		Encrypt:   sealBlock,
		Decrypt:   decryptBlock,
	})
}

// hdrLen 是统一落盘格式首个 8B BE 字段宽度（meta blob 长度头 / 块 blob 密文总长）。
// 用户裁定（2026-10-02，未上线零成本）：长度头 4B→8B，单 blob 密文上限无实用限制；
// 文件大小恒等式 = R + 8 + 32 + 密文流总长。
const hdrLen = 8

// 统一落盘格式 [R 128B][8B 密文长][salt][nonce][ct+tag] 的固定首部偏移。
const (
	// ctLenOff 是 8B 密文长度字段位置（meta blob：BE，值为 ct+tag 长度；块 blob：密文流总长）。
	ctLenOff = RandPrefixLen
	// saltOff 是 salt 段位置。
	saltOff = RandPrefixLen + hdrLen
	// nonceOff 是 nonce 段位置。
	nonceOff = RandPrefixLen + hdrLen + SaltLen
	// ctOff 是密文段起点。
	ctOff = RandPrefixLen + hdrLen + SaltLen + NonceLen
)

// 块 blob（boot+index+段序列）固定首部偏移。完整结构（最终定稿，2026-10-02 用户 21:39）：
//
//	[R 128B][8B 密文流总长][salt 32B]
//	+ [引导段 boot][索引块 index][数据段/padding段/extra段 乱序]
//
// 段边界全部不明文：每段 = [12B nonce][ct+tag]，AAD=[type 1B][off 4B][len 4B]（仅 GCM
// 认证，不落盘明文）；段间随机 padding 间隙（0-64B），长度隐含。观察者只见随机字节流。
const (
	// blSaltOff 是块 blob 内 salt 段起点。
	blSaltOff = RandPrefixLen + hdrLen
	// blListOff 是密文流起点（salt 之后；引导段 boot 固定在此）。
	blListOff = RandPrefixLen + hdrLen + SaltLen
	// bootPlainLen 是引导段明文固定长（[8B indexEncOffset][4B indexEncSize]）。
	bootPlainLen = 8 + 4
	// bootEncSize 是引导段密文总长（12B nonce + 明文 + 16B tag）。
	bootEncSize = NonceLen + bootPlainLen + 16
)

// BlockletType 是 blocklet 段类型（作 GCM AAD 一部分——type/off/len 为段定位认证信息，
// 不明文进 blob，仅解密者可见。type 分布对观察者不可见）。
type BlockletType byte

const (
	// BlockletTypeData 是数据段（文件内容；当前加密/读取主路径）。
	BlockletTypeData BlockletType = 0x01
	// BlockletTypePadding 是空闲/未使用段（打包替换复用余量；随机字节填充、仍 GCM 认证，
	// 不属于文件逻辑内容，全量还原路径跳过）。
	BlockletTypePadding BlockletType = 0x02
	// BlockletTypeExtra 是附加数据段（size:data 序列、独立加密；当前不产出，槽位预留）。
	BlockletTypeExtra BlockletType = 0x03
	// BlockletTypeBoot 是引导段（前端第一段，明文固定长，记录索引块位置）。
	BlockletTypeBoot BlockletType = 0x0F
	// BlockletTypeIndex 是索引块（记录全部段定位 + 文件级摘要，供无 meta 随机访问）。
	BlockletTypeIndex BlockletType = 0x10
	// BlockletTypeReserved 是未在逻辑中消费的类型下限（0x11；0x12/0x13 已定义预留槽位，
	// 0x14+ 全部预留；未知 type fail-closed）。
	BlockletTypeReserved BlockletType = 0x11
	// BlockletTypeRef 是去重引用块（引用其它 blob 段、不存新数据；任务 9b 预留，9c 实现，
	// 当前无消费逻辑、未知路径 fail-closed）。
	BlockletTypeRef BlockletType = 0x12
	// BlockletTypeParity 是纠错块（XOR parity，k-of-k+1 纯 stdlib；任务 9b 预留，后续实现，
	// 当前无消费逻辑、未知路径 fail-closed）。
	BlockletTypeParity BlockletType = 0x13
)

// kdfMaterial 组装 scrypt 派生输入 = secret || kdfDomain（版本域混入 secret，不明文
// 进 blob）。空域即返回 secret 原样（数学上等价；域标记非空时逐字拼接）。
func kdfMaterial(secret []byte, domain string) []byte {
	if domain == "" {
		return secret
	}
	deriv := make([]byte, 0, len(secret)+len(domain))
	deriv = append(deriv, secret...)
	deriv = append(deriv, domain...)
	return deriv
}

// deriveKey 用 scrypt 从 secret + salt 派生文件密钥（AES-256）。v 指定算法版本：
// 派生输入 = secret || kdfDomain(v)——版本域混入 secret（**不明文进 blob**，仅影响
// 派生结果），域不同 key 不同（版本分离）。未知版本 fail-closed（无法确定派生域，
// 拒绝以错误 key 解密）。
func deriveKey(secret, salt []byte, v AlgoVersion) ([]byte, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("shardseal: secret 为空（禁止空密钥派生）")
	}
	alg, ok := registry[v]
	if !ok {
		return nil, fmt.Errorf("shardseal: 未注册算法版本 %d", v)
	}
	key, err := scrypt.Key(kdfMaterial(secret, alg.KDFDomain), salt, scryptN, scryptR, scryptP, KeyLen)
	if err != nil {
		return nil, fmt.Errorf("shardseal: scrypt 派生失败: %w", err)
	}
	return key, nil
}

// newGCM 按文件密钥构造 AES-256-GCM AEAD。
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("shardseal: 文件密钥长度 %d，应为 %d", len(key), KeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("shardseal: AES 构造失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("shardseal: GCM 构造失败: %w", err)
	}
	return gcm, nil
}

// sealBlock 把 plaintext 加密为统一落盘格式 [R 128B 随机][4B 密文长][salt][nonce][ct+tag]。
// 派生 key 由调用方每文件一次 deriveKey(secret, salt, v)，本层不重复 scrypt。
func sealBlock(key, salt, plain []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	r := make([]byte, RandPrefixLen)
	if _, err := rand.Read(r); err != nil {
		return nil, fmt.Errorf("shardseal: 随机首部失败: %w", err)
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("shardseal: 随机 nonce 失败: %w", err)
	}
	// ct 长度 = len(明文)+16（含 GCM tag）——长度头与文件大小线性一致。
	ct := gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 0, ctOff+len(ct))
	out = append(out, r...)
	var lenBuf [hdrLen]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(ct)))
	out = append(out, lenBuf[:]...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// blockletEncSize 返回单个段密文总长（12B nonce + 明文 + 16B tag；随机访问定位段长用，
// 与 meta.BlockletInfo.EncSize 及索引块条目一致）。
func blockletEncSize(size int64) int64 { return NonceLen + size + 16 }

// encodeBlockletAAD 编码段头 AAD = [type 1B][off 4B][len 4B]（GCM 认证段归属、防移花接木；
// 不明文落盘，仅解密者可见）。off 为块内相对偏移（0-based）。
func encodeBlockletAAD(typ BlockletType, off, size int64) []byte {
	b := make([]byte, 1+4+4)
	b[0] = byte(typ)
	binary.BigEndian.PutUint32(b[1:5], uint32(off))
	binary.BigEndian.PutUint32(b[5:9], uint32(size))
	return b
}

// blockletType 返回 Blocklet 的段类型字节（显式 Type 优先；否则 Padding 标记 → Padding）。
func blockletType(bl Blocklet) BlockletType {
	if bl.Type != 0 {
		return bl.Type
	}
	if bl.Padding {
		return BlockletTypePadding
	}
	return BlockletTypeData
}

// validateBlocklets 校验数据段连续覆盖 [blockOffset, blockOffset+size)，非数据段（padding/
// extra）不与已用区间相交（可延伸到已用区之后；fail-closed）。
func validateBlocklets(blocklets []Blocklet, blockOffset, size int64) error {
	if len(blocklets) == 0 {
		return fmt.Errorf("shardseal: blocklet 序列为空")
	}
	cur := blockOffset
	seen := false
	for _, bl := range blocklets {
		if bl.Size <= 0 {
			return fmt.Errorf("shardseal: blocklet 大小非法 %d", bl.Size)
		}
		if bl.Offset < blockOffset {
			return fmt.Errorf("shardseal: blocklet 偏移 %d 小于块起点 %d", bl.Offset, blockOffset)
		}
		if blockletType(bl) != BlockletTypeData {
			if verr := validateNonDataPlacement(bl, blockOffset, size); verr != nil {
				return verr
			}
			continue
		}
		var uerr error
		cur, seen, uerr = advanceUsedRange(bl, blockOffset, size, cur, seen)
		if uerr != nil {
			return uerr
		}
	}
	if !seen || cur != blockOffset+size {
		return fmt.Errorf("shardseal: blocklet 覆盖 [%d,%d)，块尺寸 %d 不符", blockOffset, cur, size)
	}
	return nil
}

// validateNonDataPlacement 校验非数据段（padding/extra 预留）不与已用区间 [blockOffset,
// blockOffset+size) 相交（可延伸到已用区之后，供打包替换复用）。
func validateNonDataPlacement(bl Blocklet, blockOffset, size int64) error {
	if bl.Offset < blockOffset+size && bl.Offset+bl.Size > blockOffset {
		return fmt.Errorf("shardseal: 非数据段 [%d,%d) 与已用区间 [%d,%d) 重叠",
			bl.Offset, bl.Offset+bl.Size, blockOffset, blockOffset+size)
	}
	return nil
}

// advanceUsedRange 校验单个数据段的连续性并推进已用区间右端点（cur/seen 为运行状态）。
func advanceUsedRange(bl Blocklet, blockOffset, size, cur int64, seen bool) (int64, bool, error) {
	if bl.Offset > blockOffset+size {
		return cur, seen, fmt.Errorf("shardseal: 数据段偏移 %d 越出块 [%d,%d)", bl.Offset, blockOffset, blockOffset+size)
	}
	if !seen {
		if bl.Offset != blockOffset {
			return cur, seen, fmt.Errorf("shardseal: blocklet 首偏移 %d 应等于块偏移 %d", bl.Offset, blockOffset)
		}
		seen = true
	} else if bl.Offset != cur {
		return cur, seen, fmt.Errorf("shardseal: blocklet 偏移 %d 不连续（期望 %d）", bl.Offset, cur)
	}
	cur += bl.Size
	return cur, seen, nil
}

// blockFirstUsed 返回第一个数据 blocklet 的 Offset（块偏移入口；全非数据时回落 0，
// 由 validateBlocklets 的 seen 检查 fail-closed 兜底）。
func blockFirstUsed(blocklets []Blocklet) int64 {
	for _, bl := range blocklets {
		if blockletType(bl) == BlockletTypeData {
			return bl.Offset
		}
	}
	return 0
}

// randomFill 生成长度为 n 的加密随机字节（padding 段 / 段间间隙填充用）。
func randomFill(n int64) []byte {
	b := make([]byte, int(n))
	if _, err := rand.Read(b); err != nil {
		return make([]byte, int(n)) // crypto/rand 失败回落全零（上游错误路径可见）
	}
	return b
}

// BlobIndexEntry 是索引块内单个段的定位（无 meta 随机访问用；off 为块内相对偏移）。
type BlobIndexEntry struct {
	Type      BlockletType
	Off       int64 // 块内相对偏移（0-based，AAD 用）
	Len       int64 // 明文长
	EncOffset int   // blob 内密文起点（nonce 起点）
	EncSize   int64 // 密文总长（12+len+16）
}

// blobIndex 是索引块明文内容（GCM 加密，解密后解析）。
type blobIndex struct {
	Entries []BlobIndexEntry `json:"entries"`
	Digest  BlobDigest       `json:"digest"`
}

// BlobDigest 是索引块内的文件级摘要（chunk 数、块大小、块级 SHA256）。
type BlobDigest struct {
	ChunkCount  int    `json:"chunk_count"`
	FileSize    int64  `json:"file_size"`
	BlockSHA256 string `json:"block_sha256"`
}

// encryptBlocklets 把块明文加密为完整块 blob（最终定稿结构，2026-10-02 用户 21:39）：
//
//	[R 128B][8B 密文流总长][salt 32B]
//	+ [boot 引导段][gap][数据/padding/extra 段...][gap][index 索引块]
//
// boot 明文固定长（记录索引块位置）；index 记录全部段定位 + 文件级摘要（有 blob+secret
// 即可随机访问，不依赖外部 meta）；每段 AAD=[type][off][len] 认证（type/off/len 不明文），
// 段间随机 padding 间隙混淆边界——观察者只见随机字节流、不可切分。返回 blob 与索引条目
// （enc_offset/enc_size 供 meta 冗余记录）。algoVer 仅防御性校验算法注册。
func encryptBlocklets(key, salt []byte, blocklets []Blocklet, data []byte, algoVer AlgoVersion) ([]byte, []BlobIndexEntry, error) {
	if _, ok := registry[algoVer]; !ok {
		return nil, nil, fmt.Errorf("shardseal: 未注册算法版本 %d（blocklet 加密 fail-closed）", algoVer)
	}
	blockOffset := blockFirstUsed(blocklets)
	if err := validateBlocklets(blocklets, blockOffset, int64(len(data))); err != nil {
		return nil, nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	gaps, entries, stream := planBlockletLayout(blocklets, blockOffset)
	idxJSON, indexEncSize, indexEncOff, err := buildBlockIndex(entries, data, stream)
	if err != nil {
		return nil, nil, err
	}
	// 密文流总长 = boot + 段序列 + 索引块（含索引段密文）。
	stream += indexEncSize
	blob, err := assembleBlockBlob(gcm, salt, blocklets, data, blockOffset, gaps, stream, indexEncOff, indexEncSize, idxJSON)
	if err != nil {
		return nil, nil, err
	}
	return blob, entries, nil
}

// planBlockletLayout 计算段间随机间隙、各段 enc_offset/enc_size 与密文流总长（不写盘）。
// 布局：boot 固定 blListOff（占用 bootEncSize），随后 blocklet 段（顺序）+ 随机间隙，
// index 放段序列末。
func planBlockletLayout(blocklets []Blocklet, blockOffset int64) (gaps []int, entries []BlobIndexEntry, stream int64) {
	gaps = make([]int, len(blocklets)+1) // gaps[i]：boot(i=0)/第 i 个 blocklet 之后的间隙
	for i := range gaps {
		if i < len(gaps)-1 {
			gaps[i] = int(cryptoRandN(64))
		}
	}
	stream = bootEncSize + int64(gaps[0])
	entries = make([]BlobIndexEntry, 0, len(blocklets))
	for _, bl := range blocklets {
		entries = append(entries, BlobIndexEntry{
			Type: blockletType(bl), Off: bl.Offset - blockOffset, Len: bl.Size,
			EncOffset: blListOff + int(stream), EncSize: blockletEncSize(bl.Size),
		})
		stream += blockletEncSize(bl.Size) + int64(gaps[len(entries)])
	}
	return gaps, entries, stream
}

// buildBlockIndex 序列化索引块明文（全部段定位 + 文件级摘要）并计算其密文位置。
func buildBlockIndex(entries []BlobIndexEntry, data []byte, stream int64) (idxJSON []byte, indexEncSize int64, indexEncOff int, err error) {
	digestHex, _ := hash16(data)
	idxJSON, jerr := json.Marshal(blobIndex{
		Entries: entries,
		Digest:  BlobDigest{ChunkCount: 1, FileSize: int64(len(data)), BlockSHA256: digestHex},
	})
	if jerr != nil {
		return nil, 0, 0, fmt.Errorf("shardseal: 索引块序列化失败: %w", jerr)
	}
	indexEncSize = blockletEncSize(int64(len(idxJSON)))
	indexEncOff = blListOff + int(stream)
	return idxJSON, indexEncSize, indexEncOff, nil
}

// assembleBlockBlob 组装最终 blob：R + 8B 密文流总长 + salt + boot + 段序列 + index。
func assembleBlockBlob(gcm cipher.AEAD, salt []byte, blocklets []Blocklet, data []byte, blockOffset int64, gaps []int, stream int64, indexEncOff int, indexEncSize int64, idxJSON []byte) ([]byte, error) {
	r := make([]byte, RandPrefixLen)
	if _, err := rand.Read(r); err != nil {
		return nil, fmt.Errorf("shardseal: 随机首部失败: %w", err)
	}
	out := make([]byte, 0, blListOff+int(stream))
	out = append(out, r...)
	var totalBuf [hdrLen]byte
	binary.BigEndian.PutUint64(totalBuf[:], uint64(stream))
	out = append(out, totalBuf[:]...)
	out = append(out, salt...)
	out, err := sealBootSegment(gcm, out, indexEncOff, indexEncSize, gaps[0])
	if err != nil {
		return nil, err
	}
	for i, bl := range blocklets {
		out, err = sealBlockletSegment(gcm, out, bl, data, blockOffset, gaps[i+1])
		if err != nil {
			return nil, err
		}
	}
	indexNonce := make([]byte, NonceLen)
	if _, err := rand.Read(indexNonce); err != nil {
		return nil, fmt.Errorf("shardseal: 随机 nonce 失败: %w", err)
	}
	out = append(out, indexNonce...)
	out = append(out, gcm.Seal(nil, indexNonce, idxJSON, encodeBlockletAAD(BlockletTypeIndex, 0, int64(len(idxJSON))))...)
	return out, nil
}

// sealBootSegment 密封引导段并写入 out（AAD=[Boot][0][bootPlainLen]；明文 = 索引块位置），
// 随后写入随机间隙。
func sealBootSegment(gcm cipher.AEAD, out []byte, indexEncOff int, indexEncSize int64, gap int) ([]byte, error) {
	bootPlain := make([]byte, bootPlainLen)
	binary.BigEndian.PutUint64(bootPlain[0:8], uint64(indexEncOff))
	binary.BigEndian.PutUint32(bootPlain[8:12], uint32(indexEncSize))
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("shardseal: 随机 nonce 失败: %w", err)
	}
	out = append(out, nonce...)
	out = append(out, gcm.Seal(nil, nonce, bootPlain, encodeBlockletAAD(BlockletTypeBoot, 0, bootPlainLen))...)
	return append(out, randomFill(int64(gap))...), nil
}

// sealBlockletSegment 密封单个 blocklet 段并写入 out（数据段取原文；padding/extra 段随机
// 填充，均 GCM 认证），随后写入随机间隙。
func sealBlockletSegment(gcm cipher.AEAD, out []byte, bl Blocklet, data []byte, blockOffset int64, gap int) ([]byte, error) {
	if uint64(bl.Offset-blockOffset) > uint64(^uint32(0)) || uint64(bl.Size) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("shardseal: blocklet 偏移/大小超 uint32（off=%d len=%d，fail-closed）", bl.Offset, bl.Size)
	}
	segment := randomFill(bl.Size)
	if blockletType(bl) == BlockletTypeData {
		segment = data[bl.Offset-blockOffset : bl.Offset-blockOffset+bl.Size]
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("shardseal: 随机 nonce 失败: %w", err)
	}
	out = append(out, nonce...)
	out = append(out, gcm.Seal(nil, nonce, segment, encodeBlockletAAD(blockletType(bl), bl.Offset-blockOffset, bl.Size))...)
	return append(out, randomFill(int64(gap))...), nil
}

// decryptBlobIndex 解密块 blob 的 boot→index，返回全部段定位与文件级摘要。boot 固定位于
// blListOff（明文长固定，AAD=[Boot][0][bootPlainLen]）；index 位置由 boot 明文给出，AAD=
// [Index][0][indexLen]。8B 密文流总长与 walk 一致校验（fail-closed）。
func decryptBlobIndex(key, expectSalt, blob []byte) ([]BlobIndexEntry, BlobDigest, error) {
	if len(blob) < blListOff+bootEncSize {
		return nil, BlobDigest{}, fmt.Errorf("shardseal: 块 blob 过短（len=%d）", len(blob))
	}
	if verr := verifyBlockSalt(blob[blSaltOff:blListOff], expectSalt); verr != nil {
		return nil, BlobDigest{}, verr
	}
	total := binary.BigEndian.Uint64(blob[ctLenOff : ctLenOff+hdrLen])
	if uint64(len(blob)-blListOff) != total {
		return nil, BlobDigest{}, fmt.Errorf("shardseal: 密文流总长 %d 与 blob %d 不符", total, len(blob)-blListOff)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, BlobDigest{}, err
	}
	bootPlain, oerr := gcm.Open(nil,
		blob[blListOff:blListOff+NonceLen],
		blob[blListOff+NonceLen:blListOff+bootEncSize],
		encodeBlockletAAD(BlockletTypeBoot, 0, bootPlainLen))
	if oerr != nil {
		return nil, BlobDigest{}, fmt.Errorf("shardseal: boot 段解密失败（密钥错误或密文被篡改）: %w", oerr)
	}
	if len(bootPlain) != bootPlainLen {
		return nil, BlobDigest{}, fmt.Errorf("shardseal: boot 明文长度 %d，应为 %d", len(bootPlain), bootPlainLen)
	}
	indexEncOff := int(binary.BigEndian.Uint64(bootPlain[0:8]))
	indexEncSize := int64(binary.BigEndian.Uint32(bootPlain[8:12]))
	if indexEncOff < blListOff || indexEncSize < NonceLen+16 || indexEncOff+int(indexEncSize) > len(blob) {
		return nil, BlobDigest{}, fmt.Errorf("shardseal: 索引块位置非法（off=%d size=%d len=%d）", indexEncOff, indexEncSize, len(blob))
	}
	idxPlain, oerr2 := gcm.Open(nil,
		blob[indexEncOff:indexEncOff+NonceLen],
		blob[indexEncOff+NonceLen:indexEncOff+int(indexEncSize)],
		encodeBlockletAAD(BlockletTypeIndex, 0, indexEncSize-NonceLen-16))
	if oerr2 != nil {
		return nil, BlobDigest{}, fmt.Errorf("shardseal: 索引块解密失败（密钥错误或密文被篡改）: %w", oerr2)
	}
	var idx blobIndex
	if uerr := json.Unmarshal(idxPlain, &idx); uerr != nil {
		return nil, BlobDigest{}, fmt.Errorf("shardseal: 索引块反序列化失败: %w", uerr)
	}
	if err := validateIndexEntries(idx.Entries, blob); err != nil {
		return nil, BlobDigest{}, err
	}
	return idx.Entries, idx.Digest, nil
}

// validateIndexEntries 校验索引条目（type 已知、off/len 非负、enc 边界在 blob 内）。
func validateIndexEntries(entries []BlobIndexEntry, blob []byte) error {
	for i, e := range entries {
		switch e.Type {
		case BlockletTypeData, BlockletTypePadding, BlockletTypeExtra:
		default:
			return fmt.Errorf("shardseal: 索引条目 %d 未知 type 0x%02x（fail-closed）", i, byte(e.Type))
		}
		if e.Off < 0 || e.Len <= 0 {
			return fmt.Errorf("shardseal: 索引条目 %d off/len 非法（off=%d len=%d）", i, e.Off, e.Len)
		}
		if e.EncOffset < blListOff || e.EncSize < NonceLen+16 || e.EncOffset+int(e.EncSize) > len(blob) {
			return fmt.Errorf("shardseal: 索引条目 %d enc 越界（off=%d size=%d len=%d）", i, e.EncOffset, e.EncSize, len(blob))
		}
	}
	return nil
}

// verifyBlockSalt 校验 blob 内盐与 expectSalt 一致（防块被替换/错位）。
func verifyBlockSalt(salt, expectSalt []byte) error {
	if !bytes.Equal(salt, expectSalt) {
		return fmt.Errorf("shardseal: 分块 salt 与 meta 不一致（块被替换或损坏）")
	}
	return nil
}

// decryptBlock 全量还原块 blob：解 boot→index → 按 off 升序逐数据段独立 GCM 解并拼接
// （padding/extra 段跳过）。key 是文件级派生密钥；expectSalt 校验 blob 内盐（防替换）。
func decryptBlock(key, expectSalt, blob []byte) ([]byte, error) {
	entries, _, err := decryptBlobIndex(key, expectSalt, blob)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	sorted := append([]BlobIndexEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Off < sorted[j].Off })
	out := make([]byte, 0, len(blob))
	for _, e := range sorted {
		if e.Type != BlockletTypeData {
			continue // padding/extra 段不属于文件逻辑内容
		}
		plain, oerr := gcm.Open(nil,
			blob[e.EncOffset:e.EncOffset+NonceLen],
			blob[e.EncOffset+NonceLen:e.EncOffset+int(e.EncSize)],
			encodeBlockletAAD(e.Type, e.Off, e.Len))
		if oerr != nil {
			return nil, fmt.Errorf("shardseal: 解密失败（密钥错误或密文被篡改）: %w", oerr)
		}
		out = append(out, plain...)
	}
	return out, nil
}

// decryptBlockletAt 无 meta 随机访问：解 boot→index，定位含 targetOffset（块内相对偏移）
// 的数据段并只解该段（视频关键帧随机访问）。目标落在 padding/extra 段 → fail-closed。
func decryptBlockletAt(key, expectSalt, blob []byte, targetOffset int64) ([]byte, Blocklet, error) {
	entries, _, err := decryptBlobIndex(key, expectSalt, blob)
	if err != nil {
		return nil, Blocklet{}, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, Blocklet{}, err
	}
	for _, e := range entries {
		if e.Type == BlockletTypeData && targetOffset >= e.Off && targetOffset < e.Off+e.Len {
			plain, oerr := gcm.Open(nil,
				blob[e.EncOffset:e.EncOffset+NonceLen],
				blob[e.EncOffset+NonceLen:e.EncOffset+int(e.EncSize)],
				encodeBlockletAAD(e.Type, e.Off, e.Len))
			if oerr != nil {
				return nil, Blocklet{}, fmt.Errorf("shardseal: 解密失败（密钥错误或密文被篡改）: %w", oerr)
			}
			return plain, Blocklet{Offset: e.Off, Size: e.Len}, nil
		}
	}
	return nil, Blocklet{}, fmt.Errorf("shardseal: 目标偏移 %d 不在任何数据段内", targetOffset)
}

// parseBlock 解析统一落盘格式 [R][4B 密文长][salt][nonce][ct+tag]，返回
// salt/nonce/ct 三段。R 段只跳过不校验（仅混淆，篡改不影响解密）；长度头与文件
// 大小线性一致，不符即 fail-closed。
func parseBlock(blob []byte) (salt, nonce, ct []byte, err error) {
	if len(blob) < ctOff+16 {
		return nil, nil, nil, fmt.Errorf("shardseal: 分块过短（len=%d）", len(blob))
	}
	ctLen := int64(binary.BigEndian.Uint64(blob[ctLenOff : ctLenOff+hdrLen]))
	if ctLen < 16 || ctLen > int64(len(blob))-ctOff {
		return nil, nil, nil, fmt.Errorf("shardseal: 长度头 %d 与文件大小不符（len=%d）", ctLen, len(blob))
	}
	salt = blob[saltOff:nonceOff]
	nonce = blob[nonceOff:ctOff]
	ct = blob[ctOff : ctOff+ctLen]
	return salt, nonce, ct, nil
}

// openBlock 用 key 解密统一格式 blob（不含 salt 一致性校验，供 meta 解密用）。
func openBlock(key, blob []byte) ([]byte, error) {
	_, nonce, ct, err := parseBlock(blob)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 解密失败（密钥错误或密文被篡改）: %w", err)
	}
	return plain, nil
}

// encryptMetaJSON 加密 meta 明文：明文 = [4B jsonLen BE][metaJSON][rand padding]
// （padTarget>0 时 padding 到「整块落盘总长 = padTarget」；0 = 不 padding；过小目标
// 视为不 padding——padding 只往大里扩，绝不裁剪）。输出统一 [R][4B 密文长][salt]
// [nonce][ct+tag]，长度头与文件大小线性一致（padding 在密文内、属 GCM 认证范围）。
func encryptMetaJSON(key, salt, metaJSON []byte, padTarget int) ([]byte, error) {
	if uint64(len(metaJSON)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("shardseal: metaJSON 过长（len=%d）", len(metaJSON))
	}
	// 天然整块总长（无 padding）：R + 4B 长度头 + salt + nonce + (4B jsonLen + json + GCM tag)。
	natural := ctOff + 4 + len(metaJSON) + 16
	blobLen := natural
	if padTarget > natural {
		blobLen = padTarget
	}
	pad := blobLen - natural

	plain := make([]byte, 0, 4+len(metaJSON)+pad)
	var jsonLen [4]byte
	binary.BigEndian.PutUint32(jsonLen[:], uint32(len(metaJSON)))
	plain = append(plain, jsonLen[:]...)
	plain = append(plain, metaJSON...)
	if pad > 0 {
		r := make([]byte, pad)
		if _, err := rand.Read(r); err != nil {
			return nil, fmt.Errorf("shardseal: 随机填充失败: %w", err)
		}
		plain = append(plain, r...)
	}
	return sealBlock(key, salt, plain)
}

// decryptMetaJSON 解密 meta blob：跳 R → 长度头 → GCM → 读 4B jsonLen 截取真实
// JSON（padding 是解密明文的一部分，不进 JSON）。篡改密文（含 padding）fail-closed。
func decryptMetaJSON(key, blob []byte) ([]byte, error) {
	plain, err := openBlock(key, blob)
	if err != nil {
		return nil, err
	}
	if len(plain) < 4 {
		return nil, fmt.Errorf("shardseal: meta 明文过短（len=%d）", len(plain))
	}
	jsonLen := int(binary.BigEndian.Uint32(plain[:4]))
	if jsonLen < 0 || 4+jsonLen > len(plain) {
		return nil, fmt.Errorf("shardseal: meta jsonLen %d 越界（明文 len=%d）", jsonLen, len(plain))
	}
	return plain[4 : 4+jsonLen], nil
}

// newSalt 生成文件级随机盐。
func newSalt() ([]byte, error) {
	s := make([]byte, SaltLen)
	if _, err := rand.Read(s); err != nil {
		return nil, fmt.Errorf("shardseal: 随机盐失败: %w", err)
	}
	return s, nil
}
