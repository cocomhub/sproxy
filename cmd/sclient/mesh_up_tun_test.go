// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_up_tun_test.go 校验 mesh up --tun 内核 VPN 装配（roadmap 11.1-⑤ P1 片）：
//  1. up 命令含 --tun/--vip flag（默认关，SOCKS5 形态零回归）；
//  2. --tun 装配 fail-closed：PlatformProbe 不支持/无特权 → 明确报错不静默回落；
//  3. --tun 缺 --vip / vip 非法 → 报错。

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/vpn"
	"github.com/spf13/cobra"
)

// TestMeshUpTUN_Registered 校验 mesh up 注册 --tun/--vip flag（P1 装配面）。
func TestMeshUpTUN_Registered(t *testing.T) {
	t.Parallel()
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	root := &cobra.Command{Use: "test"}
	mesh := NewCmdMesh(clientfactory.NewMock(nil, nil), ios, nil)
	root.AddCommand(mesh)
	up := findSub(mesh, "up")
	if up == nil {
		t.Fatal("mesh 应含 up 子命令")
	}
	if up.Flags().Lookup("tun") == nil {
		t.Fatal("up 应注册 --tun flag")
	}
	if up.Flags().Lookup("vip") == nil {
		t.Fatal("up 应注册 --vip flag")
	}
	// 默认关（零回归：未指定 --tun 时走 SOCKS5 形态）。
	if up.Flags().Lookup("tun").DefValue != "false" {
		t.Fatalf("--tun 默认应为 false，got %q", up.Flags().Lookup("tun").DefValue)
	}
}

// TestMeshUpTUN_PlatformProbeFailClosed 校验 --tun 装配在平台探测失败时明确报错
// （不静默回落 SOCKS5；变异：探测失败被吞掉 → 红）。
func TestMeshUpTUN_PlatformProbeFailClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		probe func() (bool, error)
		want  string
	}{
		{
			name: "platform_unsupported",
			probe: func() (bool, error) {
				return false, vpn.ErrPlatformNotSupported
			},
			want: "暂不支持当前平台",
		},
		{
			name: "no_privilege",
			probe: func() (bool, error) {
				return false, &tunPermissionError{}
			},
			want: "设备不可用",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
			cmd := newCmdMeshUp(clientfactory.NewMock(nil, nil), ios, nil)
			cmd.SetArgs([]string{"--tun", "--vip", "100.64.0.5"})
			// 通过命令层参数注入探测桩（runMeshUpTUN 的 probe 参数；
			// 避免全局可变状态，并行测试无竞态）。
			cmd.RunE = func(c *cobra.Command, _ []string) error {
				return runMeshUpTUN(c, clientfactory.NewMock(nil, nil), ios, nil, "100.64.0.5", tc.probe)
			}
			err := cmd.Execute()
			if err == nil {
				t.Fatal("平台探测失败时 --tun 应报错（fail-closed，不静默回落）")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应包含 %q，got %q", tc.want, err.Error())
			}
		})
	}
}

// TestMeshUpTUN_MissingVIP 校验 --tun 缺 --vip 报错（fail-closed）。
func TestMeshUpTUN_MissingVIP(t *testing.T) {
	t.Parallel()
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	cmd := newCmdMeshUp(clientfactory.NewMock(nil, nil), ios, nil)
	cmd.SetArgs([]string{"--tun"})
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return runMeshUpTUN(c, clientfactory.NewMock(nil, nil), ios, nil, "", func() (bool, error) { return true, nil })
	}
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--tun 缺 --vip 应报错")
	}
	if !strings.Contains(err.Error(), "--vip") {
		t.Fatalf("错误应提示 --vip，got %q", err.Error())
	}
}

// TestMeshUpTUN_InvalidVIP 校验 --vip 非法地址报错（fail-closed）。
func TestMeshUpTUN_InvalidVIP(t *testing.T) {
	t.Parallel()
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	cmd := newCmdMeshUp(clientfactory.NewMock(nil, nil), ios, nil)
	cmd.SetArgs([]string{"--tun", "--vip", "999.1.1.1"})
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return runMeshUpTUN(c, clientfactory.NewMock(nil, nil), ios, nil, "999.1.1.1", func() (bool, error) { return true, nil })
	}
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--vip 非法应报错")
	}
}

// errTUNPermission 是测试用「无特权」探测错误。
type tunPermissionError struct{}

func (e *tunPermissionError) Error() string {
	return "vpn: 打开 /dev/net/tun 失败: permission denied（需要 root/管理员特权）"
}
