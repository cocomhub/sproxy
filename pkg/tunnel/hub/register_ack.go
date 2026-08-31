// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// ErrRegisterRejected 表示 hub 通过注册 ACK 明确拒绝本次注册（鉴权/格式错误）。
// 客户端 isTerminalRelayError 用 errors.Is 判定，可穿透任意 %w 包装，避免文案
// 改写或包装后终态判定静默失效。
var ErrRegisterRejected = errors.New("注册失败")

// CapabilityVirtualIP 是节点声明"希望 REG_OK 下发自身虚拟 IP"的能力标志。
// 声明后 hub 在 REG_OK 帧尾追加本节点虚拟 IP（线上格式 REG_OK:secret:vip，
// 见 buildRegisterAck），使 Discover=false 的 relay 出口节点 / mesh node 也能
// 立即得知自身虚拟 IP，不依赖 discovery 环拉 /api/hub/nodes（防静默失效）。
const CapabilityVirtualIP = "virtual-ip"

// RegisterAck 是解析后的注册 ACK：per-node secret 与本节点虚拟 IP。
type RegisterAck struct {
	Secret string
	// VirtualIP 为本节点虚拟 IP；未下发时为无效 Addr。
	VirtualIP netip.Addr
}

// ParseRegisterAck 解析注册 ACK 帧（xfer 层一条消息）。返回节点 per-node secret：
//   - 纯 "REG_OK"（未声明能力）→ ("", nil)；
//   - "REG_OK:<base64url secret>"（声明 per-node-secret 能力）→ (secret, nil)；
//   - "REG_OK:<secret>:<vip>"（新格式，声明 virtual-ip 能力）→ (secret, nil)，vip 段忽略；
//   - "REG_ERR:<reason>"（hub 明确拒绝）→ ("", 包装 ErrRegisterRejected 的错误)；
//   - 未知响应 → ("", 包装 ErrRegisterRejected 的错误)。
//
// 用前缀匹配而非精确比较，避免声明能力后收 "REG_OK:<secret>" 被误判未知响应
// 导致 relay start 终止（B1 复检 bug 回归锁）。
//
// 向后兼容：旧解析器只取 secret 前段（SplitN 首个冒号前），新格式
// "REG_OK:secret:vip" 下仍返回 secret；需要虚拟 IP 的调用方用 ParseRegisterAckFull。
func ParseRegisterAck(ackStr string) (secret string, err error) {
	ack, err := ParseRegisterAckFull(ackStr)
	if err != nil {
		return "", err
	}
	return ack.Secret, nil
}

// ParseRegisterAckFull 解析注册 ACK 帧并返回完整内容（secret + 虚拟 IP）：
//   - 纯 "REG_OK" → 空 RegisterAck；
//   - "REG_OK:<secret>" → secret，无虚拟 IP；
//   - "REG_OK:<secret>:<vip>" → secret + 虚拟 IP（vip 段非法 → 哨兵错误）；
//   - "REG_OK::<vip>"（secret 空但声明 virtual-ip）→ 空 secret + 虚拟 IP；
//   - "REG_ERR:<reason>" → 包装 ErrRegisterRejected 的错误；
//   - 未知响应 → 包装 ErrRegisterRejected 的错误。
func ParseRegisterAckFull(ackStr string) (RegisterAck, error) {
	switch {
	case ackStr == RegisterAckOK:
		return RegisterAck{}, nil
	case strings.HasPrefix(ackStr, RegisterAckOK+":"):
		rest := strings.TrimPrefix(ackStr, RegisterAckOK+":")
		parts := strings.SplitN(rest, ":", 2)
		secret := parts[0]
		ack := RegisterAck{Secret: secret}
		if len(parts) == 2 {
			if parts[1] == "" {
				// "REG_OK:<secret>:" —— secret 后挂空 vip 段：与旧格式语义不符，报错。
				return RegisterAck{}, fmt.Errorf("%w: 收到异常的 REG_OK（vip 段为空）", ErrRegisterRejected)
			}
			vip, perr := netip.ParseAddr(parts[1])
			if perr != nil {
				return RegisterAck{}, fmt.Errorf("%w: 收到异常的 REG_OK（vip 非法 %q）", ErrRegisterRejected, parts[1])
			}
			ack.VirtualIP = vip
		}
		if secret == "" && !ack.VirtualIP.IsValid() {
			return RegisterAck{}, fmt.Errorf("%w: 收到异常的 REG_OK（secret 为空）", ErrRegisterRejected)
		}
		return ack, nil
	case strings.HasPrefix(ackStr, RegisterAckErr):
		// 仅当 hub 显式回发 REG_ERR 帧才算"注册失败"（鉴权错误）——
		// 这是 isTerminalRelayError 唯一采信的依据。
		return RegisterAck{}, fmt.Errorf("%w: %s", ErrRegisterRejected, strings.TrimPrefix(ackStr, RegisterAckErr))
	default:
		return RegisterAck{}, fmt.Errorf("%w: 收到未知注册响应 %q", ErrRegisterRejected, ackStr)
	}
}
