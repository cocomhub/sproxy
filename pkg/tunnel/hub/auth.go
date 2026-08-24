// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
)

// RegisterProofV1Context 是 hub 节点注册 HMAC 证明的上下文串（v1）。
// 证明 = HMAC-SHA256(SK, RegisterProofV1Context + "\n" + nodeID)。
const RegisterProofV1Context = "sproxy-hub-register/v1"

// AccessKey 是 hub 准入用的 SproxySig 凭据（hub 包自建，勿 import pkg/server）。
type AccessKey struct {
	Key    string
	Secret string
}

// ErrInvalidAccessKey 是 AK 未命中或 accessKeys 未配置（fail-closed）时返回的哨兵错误。
var ErrInvalidAccessKey = errors.New("invalid access key")

// ErrInvalidAccessKeyProof 是 HMAC proof 校验失败时返回的哨兵错误。
var ErrInvalidAccessKeyProof = errors.New("invalid access key proof")

// ComputeRegisterProof 计算节点注册的 HMAC 证明：HMAC-SHA256(SK, "sproxy-hub-register/v1\n"+nodeID)。
// skHex 为 64 hex 字符的 SproxySig AccessKeySecret；nodeID 为注册帧的 NodeID（绑定
// node_id，防串用/重放）。返回 64 hex 字符。
func ComputeRegisterProof(skHex, nodeID string) (string, error) {
	sk, err := hex.DecodeString(skHex)
	if err != nil {
		return "", fmt.Errorf("compute register proof: %w", err)
	}
	if len(sk) != 32 {
		return "", fmt.Errorf("compute register proof: sk must be 32 bytes (64 hex chars)")
	}
	mac := hmac.New(sha256.New, sk)
	fmt.Fprintf(mac, "%s\n%s", RegisterProofV1Context, nodeID)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Authenticator 验证节点注册的 SproxySig AccessKey + HMAC proof。
// fail-closed：accessKeys 为空时拒绝所有注册。
type Authenticator struct {
	accessKeys []AccessKey
}

// NewAuthenticator 创建鉴权器。accessKeys 为空时创建 fail-closed 鉴权器（拒绝所有注册）。
func NewAuthenticator(accessKeys []AccessKey) *Authenticator {
	return &Authenticator{accessKeys: accessKeys}
}

// Authenticate 验证 AK 与 HMAC proof：
//  1. 空 accessKeys → ErrInvalidAccessKey（fail-closed）
//  2. 遍历按 Key constant-time 匹配；未命中 → ErrInvalidAccessKey
//  3. 命中 → 用 Secret 重算 ComputeRegisterProof(secret, nodeID)，与 proof constant-time 比对；
//     不匹配 → ErrInvalidAccessKeyProof；匹配 → nil
func (a *Authenticator) Authenticate(ak, proof, nodeID string) error {
	if len(a.accessKeys) == 0 {
		return ErrInvalidAccessKey
	}
	var matched *AccessKey
	for i := range a.accessKeys {
		if subtle.ConstantTimeCompare([]byte(ak), []byte(a.accessKeys[i].Key)) == 1 {
			matched = &a.accessKeys[i]
			break
		}
	}
	if matched == nil {
		return ErrInvalidAccessKey
	}
	want, err := ComputeRegisterProof(matched.Secret, nodeID)
	if err != nil {
		return ErrInvalidAccessKeyProof
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(proof)) != 1 {
		return ErrInvalidAccessKeyProof
	}
	return nil
}
