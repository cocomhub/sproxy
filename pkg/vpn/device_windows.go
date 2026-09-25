// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package vpn

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
)

// platformProbe 是 Windows 的 tun 能力探测：wintun 驱动加载能力。
// 返回 (true, nil) 表示平台支持（Open 仍可能因驱动缺失失败）；
// 驱动缺失/加载失败时返回明确错误（提示安装 wintun，禁静默回落）。
//
// P1 不做真设备功能：探测只为 CLI 装配期给出可观测的「支持/不支持/驱动缺失」
// 三态（设计文档「PlatformProbe 三态」）。真 wintun 驱动加载（P2）见片划分。
func platformProbe() (bool, error) {
	// P1 标注：Windows 平台标记为「支持，但 wintun 真驱动加载留 P2」。
	// 返回 (false, 明确错误) 使 CLI 装配期 fail-closed（不静默回落 SOCKS5），
	// 同时携带 wintun 提示——设计「错误处理」要求驱动缺失明确报错。
	return false, errors.New("vpn: Windows wintun 驱动加载留 P2（本期 P1 仅接口 + 平台探测，不做真设备功能；P2 将加载 wintun.dll 并提供驱动缺失提示）")
}

// newTUNDevice 是 Windows 的 TUNDevice 构造点（覆盖 device_default.go）。
// P1 标注：wintun 骨架（保留接口形状），Open 明确 fail-closed——
// 真 wintun 驱动加载留 P2（本期不做真设备功能，见设计文档片划分）。
func newTUNDevice(vip netip.Addr) TUNDevice {
	return &windowsTUN{vip: vip}
}

// windowsTUN 是 Windows 平台 TUNDevice（P1 骨架，wintun 标注）。
type windowsTUN struct {
	vip netip.Addr
}

// Open 明确返回「P1 未实现真设备」错误（fail-closed，不静默回落）。
func (d *windowsTUN) Open(_ string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("vpn: Windows wintun 真设备打开留 P2（本期 P1 仅接口 + 平台探测；P2 将加载 wintun.dll）")
}

// SetMTU 明确返回「P1 未实现」错误。
func (d *windowsTUN) SetMTU(_ int) error {
	return errors.New("vpn: Windows wintun SetMTU 留 P2（本期 P1 仅接口 + 平台探测）")
}

// Addr 返回本机虚拟 IP。
func (d *windowsTUN) Addr() netip.Addr { return d.vip }
