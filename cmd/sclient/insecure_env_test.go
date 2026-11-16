// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/spf13/cobra"
)

// newTLSFlagCmd 构造带 --insecure / --ca-file 的测试命令（模拟 root 持久 flag 在
// 子命令上的可见形态）。
func newTLSFlagCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().Bool("insecure", false, "")
	cmd.Flags().String("ca-file", "", "")
	return cmd
}

// TestApplyEnvTLSFlags_PropagatesToFlags 锁定：环境配置 insecure/ca_file 在用户未显式
// 传 flag 时注入到全局 flag，从而对所有消费 --insecure/--ca-file 的命令生效。
func TestApplyEnvTLSFlags_PropagatesToFlags(t *testing.T) {
	t.Parallel()
	cmd := newTLSFlagCmd()
	applyEnvTLSFlags(cmd, &contextcfg.Environment{Insecure: true, CAFile: "/tmp/ca.pem"})

	if v, _ := cmd.Flags().GetBool("insecure"); !v {
		t.Error("配置 insecure: true 应注入 --insecure=true")
	}
	if v, _ := cmd.Flags().GetString("ca-file"); v != "/tmp/ca.pem" {
		t.Errorf("配置 ca_file 应注入 --ca-file，实际 %q", v)
	}
}

// TestApplyEnvTLSFlags_ExplicitFlagWins 锁定：用户显式传的 flag 不被配置覆盖
// （--insecure=false 显式关闭 vs 配置 true）。
func TestApplyEnvTLSFlags_ExplicitFlagWins(t *testing.T) {
	t.Parallel()
	cmd := newTLSFlagCmd()
	if err := cmd.Flags().Set("insecure", "false"); err != nil {
		t.Fatalf("set insecure: %v", err)
	}
	applyEnvTLSFlags(cmd, &contextcfg.Environment{Insecure: true})

	if v, _ := cmd.Flags().GetBool("insecure"); v {
		t.Error("显式 --insecure=false 不应被配置覆盖")
	}
}

// TestApplyEnvTLSFlags_NilEnvAndMissingFlags 锁定：nil env 与未注册 flag 均不 panic。
func TestApplyEnvTLSFlags_NilEnvAndMissingFlags(t *testing.T) {
	t.Parallel()
	applyEnvTLSFlags(newTLSFlagCmd(), nil)                                                           // nil env → 空操作
	applyEnvTLSFlags(&cobra.Command{Use: "y"}, &contextcfg.Environment{Insecure: true, CAFile: "x"}) // 无 flag → 跳过
}
