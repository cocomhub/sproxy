// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/scrypt"
)

// 设计 §4.2：AES-256-GCM + scrypt 派生。
//
//   - 每文件随机盐（32B）→ scrypt(secret, salt) 派生文件密钥（32B）；
//   - 每块随机 nonce（12B）；
//   - 统一落盘格式（分块 + meta）：
//     [R 128B 随机首部][4B 密文长 BE][salt][nonce][ciphertext+GCMtag]。
//     首部/大小/长度头三维度均无 meta/分块特征，无法凭 file/magic/长度区分。
//     R 段仅混淆，不参与校验（篡改不影响解密）；4B 长度头 = 密文长度（len(明文)+16），
//     与文件大小线性一致：文件大小 = R + 4 + 32 + 12 + len(密文)，恒成立。
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

// 统一落盘格式 [R 128B][4B 密文长][salt][nonce][ct+tag] 的固定首部偏移。
const (
	// ctLenOff 是 4B 密文长度字段位置（BE，值为 ct+tag 长度）。
	ctLenOff = RandPrefixLen
	// saltOff 是 salt 段位置。
	saltOff = RandPrefixLen + 4
	// nonceOff 是 nonce 段位置。
	nonceOff = RandPrefixLen + 4 + SaltLen
	// ctOff 是密文段起点。
	ctOff = RandPrefixLen + 4 + SaltLen + NonceLen
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
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
	out = append(out, lenBuf[:]...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// encryptBlock 加密单个分块：返回 [R 128B][4B 密文长][salt][nonce][ct+tag]。
func encryptBlock(key, salt, plain []byte) ([]byte, error) {
	return sealBlock(key, salt, plain)
}

// parseBlock 解析统一落盘格式 [R][4B 密文长][salt][nonce][ct+tag]，返回
// salt/nonce/ct 三段。R 段只跳过不校验（仅混淆，篡改不影响解密）；长度头与文件
// 大小线性一致，不符即 fail-closed。
func parseBlock(blob []byte) (salt, nonce, ct []byte, err error) {
	if len(blob) < ctOff+16 {
		return nil, nil, nil, fmt.Errorf("shardseal: 分块过短（len=%d）", len(blob))
	}
	ctLen := int(binary.BigEndian.Uint32(blob[ctLenOff : ctLenOff+4]))
	if ctLen < 16 || ctLen > len(blob)-ctOff {
		return nil, nil, nil, fmt.Errorf("shardseal: 长度头 %d 与文件大小不符（len=%d）", ctLen, len(blob))
	}
	salt = blob[saltOff:nonceOff]
	nonce = blob[nonceOff:ctOff]
	ct = blob[ctOff : ctOff+ctLen]
	return salt, nonce, ct, nil
}

// decryptBlock 解密单个分块。key 是调用方按文件级 salt 派生一次的密钥（DecryptFile
// 每文件只派生一次，避免逐块重复 scrypt）；expectSalt 是 meta 中记录的文件级盐，
// 与块内 salt 一致性校验（防块被替换/错位）。篡改 R 段不影响（只跳不校验）。
func decryptBlock(key, expectSalt, blob []byte) ([]byte, error) {
	salt, nonce, ct, err := parseBlock(blob)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(salt, expectSalt) {
		return nil, fmt.Errorf("shardseal: 分块 salt 与 meta 不一致（块被替换或损坏）")
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
