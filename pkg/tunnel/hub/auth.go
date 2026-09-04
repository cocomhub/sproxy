// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// NewRegisterNonce 生成注册证明用的一次性 nonce（16 字节随机数，hex 编码）。
// crypto/rand 失败概率极低；万一失败回退到时间戳+进程随机（仍满足一次性语义，
// 且 hub 端按 (ak, nonce) 去重 + 窗口校验，空/重复 nonce 均 fail-closed）。
func NewRegisterNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fb%d-%d", time.Now().UnixNano(), time.Now().UnixNano()%10000000000)
	}
	return hex.EncodeToString(b[:])
}

// RegisterProofV2Context 是 hub 节点注册 HMAC 证明的上下文串（v2）。
// 证明 = HMAC-SHA256(SK, RegisterProofV2Context + "\n" + nodeID + "\n" + ts_ms + "\n" + nonce)。
// v2 引入 ts+nonce 防重放（M-6）：捕获的注册帧无法在新窗口内重放。
const RegisterProofV2Context = "sproxy-hub-register/v2"

// registerProofMaxAge 是注册证明 ts 的最大新鲜度窗口。
// 客户端与服务端时钟偏差 + 传输延迟；窗口内允许，超出拒绝。
const registerProofMaxAge = 5 * time.Minute

// AccessKey 是 hub 准入用的 SproxySig 凭据（hub 包自建，勿 import pkg/server）。
type AccessKey struct {
	Key    string
	Secret string
}

// ErrInvalidAccessKey 是 AK 未命中或 accessKeys 未配置（fail-closed）时返回的哨兵错误。
var ErrInvalidAccessKey = errors.New("invalid access key")

// ErrInvalidAccessKeyProof 是 HMAC proof 校验失败时返回的哨兵错误。
var ErrInvalidAccessKeyProof = errors.New("invalid access key proof")

// ErrStaleRegisterProof 是注册证明 ts 超出新鲜度窗口时返回的哨兵错误（防重放）。
var ErrStaleRegisterProof = errors.New("stale register proof")

// ErrReplayRegisterNonce 是注册 nonce 已被使用（重放）时返回的哨兵错误。
var ErrReplayRegisterNonce = errors.New("replayed register nonce")

// ComputeRegisterProof 计算节点注册的 HMAC 证明（v2）：
// HMAC-SHA256(SK, "sproxy-hub-register/v2\n"+nodeID+"\n"+ts+"\n"+nonce)。
// skHex 为 64 hex 字符的 SproxySig AccessKeySecret；nodeID 绑定节点防串用；
// ts 为 unix 毫秒、nonce 为一次性随机串，共同防重放。返回 64 hex 字符。
func ComputeRegisterProof(skHex, nodeID string, ts int64, nonce string) (string, error) {
	sk, err := hex.DecodeString(skHex)
	if err != nil {
		return "", fmt.Errorf("compute register proof: %w", err)
	}
	if len(sk) != 32 {
		return "", fmt.Errorf("compute register proof: sk must be 32 bytes (64 hex chars)")
	}
	mac := hmac.New(sha256.New, sk)
	fmt.Fprintf(mac, "%s\n%s\n%d\n%s", RegisterProofV2Context, nodeID, ts, nonce)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// authenticatorTestRing 是跨包（hub_test / mesh / server / cmd/sclient）测试共用的
// Ring 装配工厂：从 []AccessKey 构造 Ring（每条 AK 一条 plain alive 条目）。
// hub 内部（package hub）测试同样可用。AK 未登记或 SK 非法（非 32 字节）的条目被
// 跳过——fail-closed 语义由 Authenticator 的空 ring 兜底。
func NewRingFromAccessKeys(aks []AccessKey) *accesskey.Ring {
	ring := accesskey.NewRing()
	for _, k := range aks {
		sk, err := hex.DecodeString(k.Secret)
		if err != nil || len(sk) != 32 {
			continue
		}
		if err := ring.UpsertAK(k.Key, ""); err != nil {
			continue
		}
		_, _ = ring.AddKey(k.Key, sk, accesskey.WithMeta(accesskey.Meta{Type: "initial"}))
	}
	return ring
}

// Authenticator 验证节点注册的 SproxySig AccessKey + HMAC proof（v2，含 ts/nonce 防重放）。
// 凭据来自共享的 *accesskey.Ring（与 HTTP 认证同源，单一事实源）：
// rotate / 过期在 ring 上动态生效，无需 SetAccessKeys 同步。
// fail-closed：ring 为空或 AK 无存活条目时拒绝所有注册。
type Authenticator struct {
	ring      *accesskey.Ring
	noncePool *sproxysig.NoncePool
}

// NewAuthenticator 创建鉴权器。ring 为 nil 时视为空 ring（fail-closed，拒绝所有注册）。
func NewAuthenticator(r *accesskey.Ring) *Authenticator {
	if r == nil {
		r = accesskey.NewRing()
	}
	return &Authenticator{
		ring:      r,
		noncePool: sproxysig.NewNoncePool(),
	}
}

// Authenticate 验证 AK、HMAC proof 与新鲜度：
//  1. ring 为空或 AK 无存活条目（CoreEntry 为 nil）→ ErrInvalidAccessKey（fail-closed）
//  2. |now−ts| > registerProofMaxAge → ErrStaleRegisterProof（防重放）
//  3. nonce 已用过 → ErrReplayRegisterNonce（防重放）
//  4. 命中 → 用 entry.Secret 重算 ComputeRegisterProof(secret, nodeID, ts, nonce)，
//     与 proof constant-time 比对；不匹配 → ErrInvalidAccessKeyProof；匹配 → nil
func (a *Authenticator) Authenticate(ak, proof, nodeID string, ts int64, nonce string) error {
	entry := a.ring.CoreEntry(ak)
	if entry == nil {
		return ErrInvalidAccessKey
	}
	now := time.Now().UnixMilli()
	if diff := now - ts; diff > registerProofMaxAge.Milliseconds() || diff < -registerProofMaxAge.Milliseconds() {
		return ErrStaleRegisterProof
	}
	if a.noncePool.Seen(ak, nonce, now+registerProofMaxAge.Milliseconds()) {
		return ErrReplayRegisterNonce
	}
	want, err := ComputeRegisterProof(hex.EncodeToString(entry.SK), nodeID, ts, nonce)
	if err != nil {
		return ErrInvalidAccessKeyProof
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(proof)) != 1 {
		return ErrInvalidAccessKeyProof
	}
	return nil
}
