// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/rand"
	"fmt"
)

// 本文件把底层加密原语对外暴露为最小包装（任务 4 secretdata 布局重构消费）。
// shardseal.Meta 明文结构在本包内定义；secretdata 卷需要自行将文件 meta JSON /
// 目录 meta JSON 加密落盘（带随机填充），并在重启加载时解密重建索引——这要求
// salt/key 派生、统一格式 blob 加解密原语跨包可达。仅新增只读包装，不改既有行为。
//
// 调用方责任：key 由 DeriveKey(secret, salt, v) 派生；meta blob 内嵌 salt 由 MetaBlobSalt
// 读取（统一格式 [R][8B 密文长][salt][nonce][ct+tag]，salt 位于固定偏移）；块 blob 内嵌
// salt 由 chunkBlobSalt 读取（[R][8B 密文流总长][salt][boot][段...][index]）。

// DeriveKey 用 scrypt 从 secret + salt 派生 AES-256 文件密钥（与分块/逐文件同参）。
// v 指定算法版本（算法域分离：派生输入 = secret || kdfDomain(v)）；v1 域 =
// "shardseal/v1"（显式域标记，见 crypto.go init 的 RegisterAlgorithm，非空串兼容）。
func DeriveKey(secret, salt []byte, v AlgoVersion) ([]byte, error) {
	return deriveKey(secret, salt, v)
}

// EncryptMetaJSON 加密 meta/目录 JSON 明文到「整块落盘总长 = padTarget」（0 = 不
// padding；过小目标不裁剪，只往大里扩）。输出统一 [R][8B 密文长][salt][nonce][ct+tag]。
func EncryptMetaJSON(key, salt, metaJSON []byte, padTarget int) ([]byte, error) {
	return encryptMetaJSON(key, salt, metaJSON, padTarget)
}

// DecryptMetaJSON 解密统一格式 meta blob，返回内嵌真实 JSON（含 padding 截取）。
func DecryptMetaJSON(key, blob []byte) ([]byte, error) {
	return decryptMetaJSON(key, blob)
}

// DecryptChunkStandalone 仅凭 secret + 分块 blob 独立解密（不依赖 meta，全量还原）。
// blob 自描述：salt 内嵌固定偏移（[R][8B 密文流总长][salt][boot][段...][index]），先读
// 内嵌 salt → 按注册表试各算法版本派生密钥 → boot→index 定位全部段 → 逐数据段独立
// GCM 解拼接（版本不明文进 blob，须试派生定位；升序试 → v1 唯一版本时即单次 scrypt）。
// blob 内嵌 salt 同时作 decryptBlock 的 expectSalt（自一致，恒过内部一致性校验）。
func DecryptChunkStandalone(secret, blob []byte) ([]byte, error) {
	salt, err := chunkBlobSalt(blob)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, v := range sortedAlgoVersions() {
		key, kerr := deriveKey(secret, salt, v)
		if kerr != nil {
			lastErr = kerr
			continue
		}
		plain, derr := decryptBlock(key, salt, blob)
		if derr == nil {
			return plain, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("shardseal: 未注册任何算法版本")
	}
	return nil, fmt.Errorf("shardseal: 全部算法版本解密失败: %w", lastErr)
}

// DecryptBlockletAt 按 meta 记录的段描述（type/offset/size/enc_offset/enc_size）跳读到
// 指定段并独立 GCM 解（有 meta 的随机访问，secretdata.OpenRangeRead 消费）。AAD =
// [info.Type][info.Offset-blockOffset][info.Size]（段头只作 GCM AAD，不解密不可见）。
// 返回该段明文与其描述。key/expectSalt 由调用方按文件级派生/解码。
func DecryptBlockletAt(key, expectSalt, blob []byte, blockOffset int64, info BlockletInfo) ([]byte, Blocklet, error) {
	if len(blob) < blListOff+bootEncSize {
		return nil, Blocklet{}, fmt.Errorf("shardseal: 块 blob 过短（len=%d）", len(blob))
	}
	if verr := verifyBlockSalt(blob[blSaltOff:blListOff], expectSalt); verr != nil {
		return nil, Blocklet{}, verr
	}
	encOff := int(info.EncOffset)
	encSize := info.EncSize
	if encOff < blListOff || encSize < NonceLen+16 || encOff+int(encSize) > len(blob) {
		return nil, Blocklet{}, fmt.Errorf("shardseal: blocklet enc 越界（off=%d size=%d len=%d）", encOff, encSize, len(blob))
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, Blocklet{}, err
	}
	plain, oerr := gcm.Open(nil,
		blob[encOff:encOff+NonceLen],
		blob[encOff+NonceLen:encOff+int(encSize)],
		encodeBlockletAAD(BlockletType(info.Type), info.Offset-blockOffset, info.Size))
	if oerr != nil {
		return nil, Blocklet{}, fmt.Errorf("shardseal: 解密失败（密钥错误或密文被篡改）: %w", oerr)
	}
	return plain, Blocklet{Offset: info.Offset, Size: info.Size}, nil
}

// DecryptBlockletStandalone 无 meta 随机访问：仅凭 secret + 块 blob，解 boot→index →
// 定位含 targetOffset（块内相对偏移）的数据段并只解该段。目标落在 padding/extra 段
// fail-closed。
func DecryptBlockletStandalone(secret, blob []byte, targetOffset int64) ([]byte, error) {
	salt, err := chunkBlobSalt(blob)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, v := range sortedAlgoVersions() {
		key, kerr := deriveKey(secret, salt, v)
		if kerr != nil {
			lastErr = kerr
			continue
		}
		plain, bl, derr := decryptBlockletAt(key, salt, blob, targetOffset)
		if derr == nil {
			if bl.Offset != targetOffset && (targetOffset < bl.Offset || targetOffset >= bl.Offset+bl.Size) {
				// 防御：返回的段必须包含目标（索引定位一致性）。
				lastErr = fmt.Errorf("shardseal: 索引定位不一致（target=%d got=%+v）", targetOffset, bl)
				continue
			}
			return plain, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("shardseal: 未注册任何算法版本")
	}
	return nil, fmt.Errorf("shardseal: 全部算法版本解密失败: %w", lastErr)
}

// chunkBlobSalt 返回块 blob 内嵌的文件级盐（供按 secret 派生 key）。块 blob 首部
// [R][8B 密文流总长][salt]，salt 位于固定偏移（blListOff 为密文流起点）。
func chunkBlobSalt(blob []byte) ([]byte, error) {
	if len(blob) < blListOff+bootEncSize {
		return nil, fmt.Errorf("shardseal: 块 blob 过短（len=%d）", len(blob))
	}
	return blob[blSaltOff:blListOff], nil
}

// DecryptMetaStandalone 仅凭 secret + meta blob 独立解密（secretdata 卷重启加载
// 目录/文件 meta 用，不依赖外部版本信息）。meta.algo_version 在密文内、解密后方可
// 知，故按注册表升序试各版本派生（v1 唯一版本时即单次 scrypt）；全部失败 fail-closed。
func DecryptMetaStandalone(secret, blob []byte) ([]byte, error) {
	salt, err := MetaBlobSalt(blob)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, v := range sortedAlgoVersions() {
		key, kerr := deriveKey(secret, salt, v)
		if kerr != nil {
			lastErr = kerr
			continue
		}
		plain, derr := decryptMetaJSON(key, blob)
		if derr == nil {
			return plain, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("shardseal: 未注册任何算法版本")
	}
	return nil, fmt.Errorf("shardseal: 全部算法版本解密失败: %w", lastErr)
}

// MetaBlobSalt 返回统一格式 blob 内嵌的文件级盐（供按 secret 派生 key）。
func MetaBlobSalt(blob []byte) ([]byte, error) {
	salt, _, _, err := parseBlock(blob)
	if err != nil {
		return nil, err
	}
	return salt, nil
}

// EncryptChunkStandalone 把一段明文加密为「单数据 blocklet」的块 blob，用**给定文件级
// key/salt**（与其它分块一致，保持 blob 内 salt == meta.Salt 的一致性校验可过）。用于
// 纠错（XOR parity，任务 9d）恢复缺失分块后重建 chunk blob：恢复出的明文重加密为同格式
// blob，DecryptFile 按统一路径读取（长度 == meta.OrigSize、整文件 SHA-256 全量校验）。
func EncryptChunkStandalone(key, salt, plain []byte, v AlgoVersion) ([]byte, error) {
	blocklets := []Blocklet{{Offset: 0, Size: int64(len(plain))}}
	blob, _, err := encryptBlocklets(key, salt, blocklets, plain, v)
	return blob, err
}

// RandSalt 生成 SaltLen 字节加密随机盐（目录 meta / 独立 blob 使用）。
func RandSalt() ([]byte, error) {
	s := make([]byte, SaltLen)
	if _, err := rand.Read(s); err != nil {
		return nil, fmt.Errorf("shardseal: 随机盐失败: %w", err)
	}
	return s, nil
}

// RandN 返回 [0, n) 加密均匀随机 int64（meta pad 目标抖动用）。
func RandN(n int64) int64 { return cryptoRandN(n) }

// RandID62 生成 9 字符随机 base62 ID（目录 meta 的 dir_id；与文件名中段同字符集，
// 匿名性一致——16hex 的 dir_id 会在 DirMetaName 中段留下 hex 密度指纹）。
func RandID62() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("shardseal: 随机 ID 失败: %w", err)
	}
	return encode62(b), nil
}
