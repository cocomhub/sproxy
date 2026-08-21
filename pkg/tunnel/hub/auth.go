// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import "fmt"

// Authenticator 验证中继节点的注册 token。
// 支持共享密钥模式：relayToken 为空时不鉴权，非空时节点必须携带相同 token。
type Authenticator struct {
	relayToken string
}

// ErrInvalidToken 是 token 校验失败时返回的哨兵错误。
var ErrInvalidToken = fmt.Errorf("invalid relay token")

// NewAuthenticator 创建鉴权器。relayToken 为空表示不鉴权。
func NewAuthenticator(relayToken string) *Authenticator {
	return &Authenticator{relayToken: relayToken}
}

// Authenticate 验证 token。token 为空且 relayToken 非空时拒绝。
func (a *Authenticator) Authenticate(token string) error {
	if a.relayToken != "" && token != a.relayToken {
		return ErrInvalidToken
	}
	return nil
}
