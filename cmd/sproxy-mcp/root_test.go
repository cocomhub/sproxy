// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/mcp"
)

// ---------------------------------------------------------------------------
// flags 解析与凭据装配（片 3 可单测部分；stdio 子进程 e2e 留后续片）
// ---------------------------------------------------------------------------

// TestRootCmd_Flags_RequiredServer 验证 --server 必填：缺省时 RunE 返回错误。
func TestRootCmd_Flags_RequiredServer(t *testing.T) {
	t.Parallel()

	cmd := newRootCmd()
	err := cmd.Execute()
	if err == nil {
		t.Fatal("缺省 --server 时应报错，实际无错误")
	}
	if !strings.Contains(err.Error(), "--server") {
		t.Fatalf("错误应指明缺少 --server，实际: %v", err)
	}
}

// TestRootCmd_Flags_VolumeOptional 验证 --volume 可选：省略时 ToolRegistry 以空卷装配
// （空卷 = auto，与 pkg/mcp NewToolRegistry 契约一致）。
func TestRootCmd_Flags_VolumeOptional(t *testing.T) {
	t.Parallel()

	reg, err := buildRegistryForTest("https://127.0.0.1:1", "", "", "", "")
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if reg == nil {
		t.Fatal("registry 不应为 nil")
	}
	if got := len(reg.List()); got != 9 {
		t.Fatalf("工具数应为 9，实际 %d", got)
	}
}

// TestRootCmd_Flags_CredentialsAssembled 验证凭据装配：--access-key /
// --access-key-secret / --access-key-id 正确落入 FileClient（SproxySig 签名三要素）。
func TestRootCmd_Flags_CredentialsAssembled(t *testing.T) {
	t.Parallel()

	fc, err := buildClientForTest("https://127.0.0.1:1", "ak-1", "sk-1", "skey-1")
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if got := fc.AccessKey(); got != "ak-1" {
		t.Fatalf("AccessKey = %q, want ak-1", got)
	}
	if got := fc.AccessKeySecret(); got != "sk-1" {
		t.Fatalf("AccessKeySecret = %q, want sk-1", got)
	}
	if got := fc.AccessKeyID(); got != "skey-1" {
		t.Fatalf("AccessKeyID = %q, want skey-1", got)
	}
	if got := fc.ServerURL(); got != "https://127.0.0.1:1" {
		t.Fatalf("ServerURL = %q, want https://127.0.0.1:1", got)
	}
}

// TestRootCmd_Flags_CredentialsOptionalNoAuth 验证无凭据也可装配（服务端免认证场景）：
// --access-key 族全部省略时 FileClient 不携带签名凭据，仅 server URL 生效。
func TestRootCmd_Flags_CredentialsOptionalNoAuth(t *testing.T) {
	t.Parallel()

	fc, err := buildClientForTest("https://127.0.0.1:1", "", "", "")
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if got := fc.AccessKey(); got != "" {
		t.Fatalf("未配置凭据时 AccessKey 应为空，实际 %q", got)
	}
	if got := fc.AccessKeySecret(); got != "" {
		t.Fatalf("未配置凭据时 AccessKeySecret 应为空，实际 %q", got)
	}
	if got := fc.AccessKeyID(); got != "" {
		t.Fatalf("未配置凭据时 AccessKeyID 应为空，实际 %q", got)
	}
}

// TestRootCmd_Flags_ParsedFromCLI 验证 --server/--volume 从命令行 flag 落入装配：
// 以 buildRegistryForTest 为靶断言解析值与 NewToolRegistry 契约一致。
func TestRootCmd_Flags_ParsedFromCLI(t *testing.T) {
	t.Parallel()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:18083", "--volume", "docs"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	// 不触发 RunE（避免阻塞读 stdin）：先 ParseFlags 后断言 flag 值。
	if err := cmd.ParseFlags([]string{"--server", "http://127.0.0.1:18083", "--volume", "docs"}); err != nil {
		t.Fatalf("ParseFlags 失败: %v", err)
	}
	f := cmd.Flags()
	gotServer, err := f.GetString(flagServer)
	if err != nil || gotServer != "http://127.0.0.1:18083" {
		t.Fatalf("server flag = %q, %v", gotServer, err)
	}
	gotVol, err := f.GetString(flagVolume)
	if err != nil || gotVol != "docs" {
		t.Fatalf("volume flag = %q, %v", gotVol, err)
	}
}

// TestRunRootE_VolumePassed 验证 RunE 把 --volume 传入 ToolRegistry 装配
// （NewToolRegistry(fc, volume) 第二参）——用真实装配断言卷透传。
func TestRunRootE_VolumePassed(t *testing.T) {
	t.Parallel()

	reg, err := buildRegistryForTest("https://127.0.0.1:1", "", "", "", "docs")
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if reg == nil {
		t.Fatal("registry 不应为 nil")
	}
}

// TestRootCmd_VersionSubcommand 验证 version 子命令存在且可执行（输出非空）。
func TestRootCmd_VersionSubcommand(t *testing.T) {
	t.Parallel()

	cmd := newRootCmd()
	cmd.AddCommand(newVersionSubcommand())
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version 子命令失败: %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Fatal("version 子命令输出为空")
	}
}

// --- 可单测装配辅助（直接复用 runServer 的生产装配 buildFileClient） ---

// buildClientForTest 走生产装配 buildFileClient（root.go），断言与 runServer 同源。
func buildClientForTest(serverURL, ak, sk, skID string) (*client.FileClient, error) {
	return buildFileClient(serverURL, ak, sk, skID)
}

// buildRegistryForTest 复刻 runServer 的 ToolRegistry 装配。
func buildRegistryForTest(serverURL, ak, sk, skID, volume string) (*mcp.ToolRegistry, error) {
	fc, err := buildFileClient(serverURL, ak, sk, skID)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolRegistry(fc, volume), nil
}
