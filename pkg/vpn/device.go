// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vpn

import (
	"errors"
	"net/netip"
)

// ErrPlatformNotSupported 是平台不支持（无 tun 能力）的哨兵错误。
// PlatformProbe 返回 (false, ErrPlatformNotSupported)；CLI 据此明确报错，
// 不静默回落 SOCKS5（禁静默降级，设计「错误处理」）。
var ErrPlatformNotSupported = errors.New("vpn: 当前平台不支持 tun/tap（仅 Linux / Windows wintun）")

// probeTUN 是 PlatformProbe 的平台实现注入点（默认调用平台探测实现；
// 测试可替换桩——device_test.go 的 withProbeStub / SetProbeTUNForTest）。
//
// 平台实现按 build tag 覆盖同名函数（platform_unsupported.go /
// device_linux.go / device_windows.go 各自定义 platformProbe），
// 未支持平台默认返回 ErrPlatformNotSupported。
var probeTUN = platformProbe

// SetProbeTUNForTest 替换 PlatformProbe 的平台探测桩（**仅供测试**：
// cmd/sclient 装配测试经此注入，避免依赖本机真实 tun 能力）。
// 调用方负责恢复（t.Cleanup）。
func SetProbeTUNForTest(fn func() (bool, error)) {
	probeTUN = fn
}

// RestoreProbeTUN 恢复平台探测为默认实现（仅供测试 cleanup 使用）。
func RestoreProbeTUN() { probeTUN = platformProbe }

// PlatformProbe 探测当前平台是否可创建 tun 设备（三态）：
//   - (true, nil)          → 支持且可探测（Open 仍可能因特权失败）；
//   - (false, ErrPlatformNotSupported) → 平台不支持（未支持平台不参与构建时，
//     由默认实现返回本哨兵）；
//   - (false, err)         → 平台支持但设备不可用（无特权 EPERM / 驱动缺失）。
//
// 任何失败态都不静默回落用户态 SOCKS5——由 CLI 装配层把错误映射为用户可见提示
// （sudo/管理员、wintun 驱动缺失）。
func PlatformProbe() (bool, error) {
	return probeTUN()
}

// NewTUNDevice 构造平台 TUNDevice 实现。
//
// 平台实现经 build tag 隔离，各平台在 newTUNDevice 中返回自己的实现：
//   - device_linux.go：Linux tun 骨架（P1 不做真设备，Open fail-closed）；\n" +
//   - device_windows.go：Windows wintun 标注（P2 真驱动加载）；
//   - platform_unsupported.go：未支持平台 fail-closed stub。
//
// 任意平台下 TUNDevice 接口均可编译（Open/SetMTU 明确报错，不静默回落
// SOCKS5——禁静默降级）。
func NewTUNDevice(vip netip.Addr) TUNDevice {
	return newTUNDevice(vip)
}
