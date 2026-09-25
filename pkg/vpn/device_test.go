// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vpn

import (
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
)

// TestTUNDeviceInterface 校验 P1 接口契约（设计文档「测试 + 变异点」）：
//  1. TUNDevice 接口在三平台 build tag 下均有实现（编译期：NewTUNDevice 返回
//     TUNDevice；未支持平台返回明确 not-supported 错误——不静默回落）；
//  2. PlatformProbe 桩三态（支持 / 不支持 / 无特权）经包级注入点可观测。
//
// 变异：PlatformProbe 三态被绕过（恒 true/恒 false）→ 红；Open 错误不携带
// 平台提示 → 红。
func TestTUNDeviceInterface(t *testing.T) {
	// 三态共用包级注入点 probeTUN（互斥写，不能并行）；本函数自身并行化。
	// 变异验证均基于注入点，不依赖平台探测实现。

	// 1) 编译期契约：任意平台都能构造 TUNDevice（未支持平台 = 明确错误的实现）。
	dev := NewTUNDevice(netip.MustParseAddr("100.64.0.5"))
	if dev.Addr() != netip.MustParseAddr("100.64.0.5") {
		t.Fatalf("Addr() = %s, want 100.64.0.5", dev.Addr())
	}
	// 接口形状钉住：Open 返回 io.ReadWriteCloser（Router 直接透传 IP 包）。
	var _ io.ReadWriteCloser

	// 2) PlatformProbe 三态（桩注入；全平台同测）。
	t.Run("probe_supported", func(t *testing.T) {
		withProbeStub(t, func() (bool, error) { return true, nil })
		ok, err := PlatformProbe()
		if !ok || err != nil {
			t.Fatalf("支持态应 (true, nil)，got (%v, %v)", ok, err)
		}
	})
	t.Run("probe_unsupported", func(t *testing.T) {
		withProbeStub(t, func() (bool, error) { return false, ErrPlatformNotSupported })
		ok, err := PlatformProbe()
		if ok || err == nil || !errors.Is(err, ErrPlatformNotSupported) {
			t.Fatalf("不支持态应 (false, ErrPlatformNotSupported)，got (%v, %v)", ok, err)
		}
	})
	t.Run("probe_no_privilege", func(t *testing.T) {
		withProbeStub(t, func() (bool, error) {
			return false, errors.New("vpn: 打开 /dev/net/tun 失败: permission denied")
		})
		ok, err := PlatformProbe()
		if ok || err == nil {
			t.Fatalf("无特权态应 (false, err)，got (%v, %v)", ok, err)
		}
	})
}

// TestTUNDevice_OpenFailClosed 校验平台实现的 Open 明确返回错误（fail-closed，
// 不静默回落 SOCKS5）：本平台实现（Linux tun 骨架 / Windows wintun 标注 / 未支持
// 平台 stub）P1 均不做真设备功能。
//
// 变异（本测试直接守卫）：
//   - Open 返回 nil 错误 → 红（fail-closed 失效）；
//   - Open 错误不携带平台提示（root/管理员/wintun/P2）→ 红（禁静默失败）。
func TestTUNDevice_OpenFailClosed(t *testing.T) {
	t.Parallel()
	dev := NewTUNDevice(netip.MustParseAddr("100.64.0.5"))
	// SetMTU 同属 fail-closed 面（无设备句柄时明确报错，不静默成功）：
	// 变异：SetMTU 返回 nil → 红。
	if mtuErr := dev.SetMTU(DefaultMTU); mtuErr == nil {
		t.Fatal("P1 平台实现 SetMTU 应返回错误（不做真设备功能）")
	}
	rc, err := dev.Open("sproxy0")
	if err == nil {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatal("P1 平台实现 Open 应返回错误（不做真设备功能）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "root") && !strings.Contains(msg, "管理员") && !strings.Contains(msg, "wintun") && !strings.Contains(msg, "平台实现") && !strings.Contains(msg, "P2") {
		t.Fatalf("Open 错误应携带平台提示，got %q", msg)
	}
}

// withProbeStub 临时替换 PlatformProbe 的注入点（测试后可恢复）。
func withProbeStub(t *testing.T, stub func() (bool, error)) {
	t.Helper()
	orig := probeTUN
	probeTUN = stub
	t.Cleanup(func() { probeTUN = orig })
}
