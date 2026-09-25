// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package vpn

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
)

// platformProbe 是 Linux 的 tun 能力探测：检查 /dev/net/tun 是否存在且可访问。
// 返回 (true, nil) 表示平台支持（Open 仍可能因特权失败）；
// 无特权（EPERM）时返回明确错误（提示 sudo/root，禁静默回落）。
//
// P1 不做真设备功能：探测只为 CLI 装配期给出可观测的「支持/不支持/无特权」
// 三态（设计文档「PlatformProbe 三态」）。真设备打开/MTU 设置留 P2/P3。
func platformProbe() (bool, error) {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return false, fmt.Errorf("vpn: 打开 /dev/net/tun 失败: %w（需要 root/sudo 特权）", err)
		}
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("vpn: /dev/net/tun 不存在（需要内核 TUN 支持，如 modprobe tun）: %w", err)
		}
		return false, fmt.Errorf("vpn: 打开 /dev/net/tun 失败: %w", err)
	}
	_ = f.Close()
	return true, nil
}

// newTUNDevice 是 Linux 的 TUNDevice 构造点（覆盖 device_default.go）。
// P1：tun 骨架（保留接口形状），Open 明确 fail-closed——真设备打开（ioctl
// TUNSETIFF）+ SetMTU + 地址装配留 P2（本期不做真设备功能，见设计文档片划分）。
func newTUNDevice(vip netip.Addr) TUNDevice {
	return &linuxTUN{vip: vip}
}

// linuxTUN 是 Linux 平台 TUNDevice（P1 骨架）。
type linuxTUN struct {
	vip netip.Addr
}

// Open 明确返回「P1 未实现真设备」错误（fail-closed，不静默回落）。
func (d *linuxTUN) Open(_ string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("vpn: Linux tun 真设备打开留 P2（本期 P1 仅接口 + 平台探测，不做真设备功能）")
}

// SetMTU 明确返回「P1 未实现」错误。
func (d *linuxTUN) SetMTU(_ int) error {
	return errors.New("vpn: Linux tun SetMTU 留 P2（本期 P1 仅接口 + 平台探测）")
}

// Addr 返回本机虚拟 IP。
func (d *linuxTUN) Addr() netip.Addr { return d.vip }
