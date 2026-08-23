// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"errors"
	"fmt"
	"strings"
)

// ErrRegisterRejected 表示 hub 通过注册 ACK 明确拒绝本次注册（鉴权/格式错误）。
// 客户端 isTerminalRelayError 用 errors.Is 判定，可穿透任意 %w 包装，避免文案
// 改写或包装后终态判定静默失效。
var ErrRegisterRejected = errors.New("注册失败")

// ParseRegisterAck 解析注册 ACK 帧（xfer 层一条消息）。返回节点 per-node secret：
//   - 纯 "REG_OK"（未声明能力）→ ("", nil)；
//   - "REG_OK:<base64url secret>"（声明 per-node-secret 能力）→ (secret, nil)；
//   - "REG_ERR:<reason>"（hub 明确拒绝）→ ("", 包装 ErrRegisterRejected 的错误)；
//   - 未知响应 → ("", 包装 ErrRegisterRejected 的错误)。
//
// 用前缀匹配而非精确比较，避免声明能力后收 "REG_OK:<secret>" 被误判未知响应
// 导致 relay start 终止（B1 复检 bug 回归锁）。
func ParseRegisterAck(ackStr string) (secret string, err error) {
	switch {
	case ackStr == RegisterAckOK:
		return "", nil
	case strings.HasPrefix(ackStr, RegisterAckOK+":"):
		secret = strings.TrimPrefix(ackStr, RegisterAckOK+":")
		if secret == "" {
			return "", fmt.Errorf("%w: 收到异常的 REG_OK（secret 为空）", ErrRegisterRejected)
		}
		return secret, nil
	case strings.HasPrefix(ackStr, RegisterAckErr):
		// 仅当 hub 显式回发 REG_ERR 帧才算"注册失败"（鉴权错误）——
		// 这是 isTerminalRelayError 唯一采信的依据。
		return "", fmt.Errorf("%w: %s", ErrRegisterRejected, strings.TrimPrefix(ackStr, RegisterAckErr))
	default:
		return "", fmt.Errorf("%w: 收到未知注册响应 %q", ErrRegisterRejected, ackStr)
	}
}
