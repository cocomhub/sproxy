// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// newHubServerFlagCmd 构造「根命令持 persistent server/access-key* flag + 子命令持 --hub」的最小
// 结构，用于验证 getHubServerURL 的解析顺序。
//
// 自 cloud_list_test.go 的 getCloudServerURL 用例迁入：原助手已删（无生产调用方，`make deadcode`
// 报 unreachable），仍在服役的同源逻辑是 relay 族的 getHubServerURL，其用例归属本文件。
func newHubServerFlagCmd() *cobra.Command {
	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("access-key", "", "")
	root.PersistentFlags().String("access-key-secret", "", "")
	root.PersistentFlags().String("access-key-id", "", "")

	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("hub", "", "")
	root.AddCommand(cmd)
	return cmd
}

func TestGetHubServerURL_FromFlag(t *testing.T) {
	t.Parallel()
	cmd := newHubServerFlagCmd()
	root := cmd.Root()
	root.PersistentFlags().Set("server", "http://test-server:18083")
	root.PersistentFlags().Set("access-key", "test-ak")
	root.PersistentFlags().Set("access-key-secret", "test-sk")

	serverURL, ak, sk, _ := getHubServerURL(cmd, nil)
	if serverURL != "http://test-server:18083" {
		t.Errorf("server URL = %q, want 来自 flag", serverURL)
	}
	if ak != "test-ak" || sk != "test-sk" {
		t.Errorf("凭据 = %q/%q, want 来自 flag", ak, sk)
	}
}

func TestGetHubServerURL_FromConfig(t *testing.T) {
	t.Parallel()
	cmd := newHubServerFlagCmd()
	cfgSvc := &testConfigProvider{cfg: &client.Config{ServerURL: "http://cfg-server:18083", AccessKey: "cfg-ak", AccessKeySecret: "cfg-sk"}}

	serverURL, ak, sk, _ := getHubServerURL(cmd, cfgSvc)
	if serverURL != "http://cfg-server:18083" {
		t.Errorf("server URL = %q, want 来自配置", serverURL)
	}
	if ak != "cfg-ak" || sk != "cfg-sk" {
		t.Errorf("凭据 = %q/%q, want 来自配置", ak, sk)
	}
}

func TestGetHubServerURL_FlagOverridesConfig(t *testing.T) {
	t.Parallel()
	cmd := newHubServerFlagCmd()
	root := cmd.Root()
	root.PersistentFlags().Set("server", "http://flag-server:18083")
	root.PersistentFlags().Set("access-key", "flag-ak")
	root.PersistentFlags().Set("access-key-secret", "flag-sk")
	cfgSvc := &testConfigProvider{cfg: &client.Config{ServerURL: "http://cfg-server:18083", AccessKey: "cfg-ak", AccessKeySecret: "cfg-sk"}}

	serverURL, ak, sk, _ := getHubServerURL(cmd, cfgSvc)
	if serverURL != "http://flag-server:18083" {
		t.Errorf("server URL = %q, want flag 覆盖配置", serverURL)
	}
	if ak != "flag-ak" || sk != "flag-sk" {
		t.Errorf("凭据 = %q/%q, want flag 覆盖配置", ak, sk)
	}
}

// TestGetHubServerURL_HubFlagMapping 覆盖 --hub → HTTP 管理地址的派生：
// ws:// → http://、wss:// → https://，且丢弃 path/query（Hub 的 ws 端点路径不是 API 基址）。
func TestGetHubServerURL_HubFlagMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		hub  string
		want string
	}{
		{hub: "ws://hub.example.com:18083/ws", want: "http://hub.example.com:18083"},
		{hub: "wss://hub.example.com/ws", want: "https://hub.example.com"},
		{hub: "http://127.0.0.1:18083/ignored", want: "http://127.0.0.1:18083"},
	}
	for _, tc := range cases {
		t.Run(tc.hub, func(t *testing.T) {
			t.Parallel()
			cmd := newHubServerFlagCmd()
			if err := cmd.Flags().Set("hub", tc.hub); err != nil {
				t.Fatalf("设置 --hub: %v", err)
			}
			serverURL, _, _, _ := getHubServerURL(cmd, nil)
			if serverURL != tc.want {
				t.Fatalf("--hub %q → %q, want %q", tc.hub, serverURL, tc.want)
			}
		})
	}
}

// TestGetHubServerUR_ServerBeatsHub 明确优先级：显式 --server 优先于 --hub。
func TestGetHubServerUR_ServerBeatsHub(t *testing.T) {
	t.Parallel()
	cmd := newHubServerFlagCmd()
	cmd.Root().PersistentFlags().Set("server", "http://explicit:18083")
	if err := cmd.Flags().Set("hub", "ws://hub.example.com/ws"); err != nil {
		t.Fatalf("设置 --hub: %v", err)
	}
	if got, _, _, _ := getHubServerURL(cmd, nil); got != "http://explicit:18083" {
		t.Fatalf("server URL = %q, want 显式 --server 优先", got)
	}
}
