// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
)

const (
	// remoteReadIKMPrefix 是远程只读静态密钥 HKDF 的域分离 IKM 前缀。
	remoteReadIKMPrefix = "sproxy-remote-read/v1|"
	// remoteReadStaticInfo 是该 HKDF 的 info（与 ecdhInfo/ecdhInfoStatic 区分）。
	remoteReadStaticInfo = "sproxy-remote-read/static-key"
)

// DeriveRemoteStaticKey 由 **listener 自己的 Ed25519 身份指纹**确定性派生远程只读
// 隧道的静态密钥；两端（A 从 pin 配置、B 从自己的身份）算出同一结果，零额外配置。
//
// 规格修正（见计划「修正 1」）：规格 AD-3 原式取 min/max(nodeA,nodeB) 为 IKM，但 B 在
// 握手完成前无法得知对端 node-id（静态密钥是握手的输入，对端身份是握手的输出），构成
// 循环依赖。改用 listener 自己的身份指纹——它正是 A 侧已经 pin 的信任锚。
//
// 安全论证：该密钥在 deriveSessionKey 中**只作 HKDF salt**，salt 不需要保密；机密性
// 来自 ECDH sharedSecret（前向保密），真实性由 AD-2 的双向 Ed25519 身份指纹 pin 保证
// （远程只读面两端都配置 WithPeerFingerprints；未配 pin 时对端身份不受本端信任锚约束）。
// 此值提供域分离，把远程读的会话密钥与其它隧道用途隔开。
//
// 返回 nil 的情况在 Tunnel 中意味着「明文模式」（key == nil 时短路：不握手、不加密），
// 即静默降级为不加密——因此此处对不可达错误 panic（fail-closed），绝不返回 nil。
//
// **该值不是秘密，且「只作 salt」并不完整**：Tunnel 在 dialer 侧握手失败且**未配置 pin**
// 时只记 Warn 并继续（tunnel_mux.go 的 default 分支），此时 sessionKey 仍为 nil，随后
// encryptionKey() **回退返回 t.key**——也就是这个「由公开的 listener 身份指纹推导」的值
// 会成为实际加密密钥。当前远程只读面尚不可利用（listener 侧 Serve 握手失败即返回错误，
// 根本不进 accept 循环），但这是 T5 接线的硬前提：**双向 pin 是 fail-closed 的必要条件**，
// 任一端漏配 pin，就会退化成「用公开可推导的密钥加密」。
//
// TODO(T5)：远程只读 listener 接线时必须保证两端都配置 WithPeerFingerprints（A pin B、
// B pin A），否则上述回退路径会把本值当加密密钥用。
func DeriveRemoteStaticKey(listenerFingerprint string) []byte {
	if listenerFingerprint == "" {
		panic("tunnel: DeriveRemoteStaticKey 需要非空 listener 指纹（fail-closed）")
	}
	ikm := []byte(remoteReadIKMPrefix + listenerFingerprint)
	key, err := hkdf.Key(sha256.New, ikm, nil, remoteReadStaticInfo, sessionKeyLen)
	if err != nil {
		// 不可达：hkdf.Key 仅在 keyLen 非正或 > 255*hashLen 时报错，sessionKeyLen(32) 恒合法。
		panic(fmt.Sprintf("tunnel: 派生远程只读静态密钥失败: %v", err))
	}
	return key
}
