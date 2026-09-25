// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

package vpn

import (
	"fmt"
	"io"
	"net/netip"
)

// platformProbe 是未支持平台的探测实现：明确返回「平台不支持」哨兵，
// 不参与 tun 能力探测（不产生 EPERM 误报，也不静默回落 SOCKS5）。
//
// Linux 真实现（device_linux.go）与 Windows wintun 标注（device_windows.go）
// 各自覆盖本文件的两个函数；未支持平台（darwin/bsd 等）保持本 fail-closed 默认。
func platformProbe() (bool, error) {
	return false, ErrPlatformNotSupported
}

// newTUNDevice 是平台 TUNDevice 构造点（NewTUNDevice 转发）。
//
// 平台实现经 build tag 覆盖本函数：
//   - device_linux.go：Linux tun 骨架（P1 不做真设备，Open fail-closed）；
//   - device_windows.go：Windows wintun 标注（P2 真驱动加载）；
//   - 本文件（未支持平台）：fail-closed stub（Open/SetMTU 明确报错，
//     不静默回落 SOCKS5——禁静默降级）。
func newTUNDevice(vip netip.Addr) TUNDevice {
	return &unsupportedTUN{vip: vip}
}

// unsupportedTUN 是未支持平台的 fail-closed 实现（P1：不做真设备功能）。
// 也作为 Linux/Windows 平台实现的共同兜底形态（各自 newTUNDevice 可复用）。
type unsupportedTUN struct {
	vip netip.Addr
}

// Open 明确返回「平台不支持」错误（附特权/驱动提示；不静默回落）。
func (d *unsupportedTUN) Open(_ string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("%w：当前构建未包含 tun 平台实现（Linux tun / Windows wintun 需 P2 装配）", ErrPlatformNotSupported)
}

// SetMTU 明确返回「平台不支持」错误（无设备句柄可设置）。
func (d *unsupportedTUN) SetMTU(_ int) error {
	return ErrPlatformNotSupported
}

// Addr 返回本机虚拟 IP（CLI 装配期展示/路由用；未支持平台仅占位）。
func (d *unsupportedTUN) Addr() netip.Addr { return d.vip }
