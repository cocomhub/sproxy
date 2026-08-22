// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import "fmt"

// Authenticator 验证中继节点的注册 token。
// fail-closed：relayToken 为空时拒绝所有 token，防止未配置共享密钥的 hub 开放注册
// （C2 纵深加固：即使 Config.Validate 被绕过，/ws 也默认关闭注册）。
type Authenticator struct {
	relayToken string
}

// ErrInvalidToken 是 token 校验失败时返回的哨兵错误。
var ErrInvalidToken = fmt.Errorf("invalid relay token")

// NewAuthenticator 创建鉴权器。relayToken 为空时创建 fail-closed 鉴权器（拒绝所有 token）。
func NewAuthenticator(relayToken string) *Authenticator {
	return &Authenticator{relayToken: relayToken}
}

// Authenticate 验证 token。fail-closed：relayToken 为空或 token 不匹配时一律拒绝。
func (a *Authenticator) Authenticate(token string) error {
	if a.relayToken == "" || token != a.relayToken {
		return ErrInvalidToken
	}
	return nil
}
