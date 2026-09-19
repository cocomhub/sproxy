// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory_test

import (
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/spf13/cobra"
)

// contextCmd 构造 factory.NewClient 所需的最小 flag 集（与 newDirectCmd 对齐）。
func contextCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	cmd.Flags().String("ca-file", "", "")
	cmd.Flags().Bool("insecure", false, "")
	cmd.Flags().Bool("allow-transport-fallback", false, "")
	cmd.Flags().Int64("chunk-size", 0, "")
	cmd.Flags().String("volume", "", "")
	cmd.Flags().String("access-key", "", "")
	cmd.Flags().String("access-key-secret", "", "")
	cmd.Flags().String("access-key-id", "", "")
	cmd.Flags().String("client-cert", "", "")
	cmd.Flags().String("client-key", "", "")
	cmd.Flags().Bool("client-cert-allow-missing", false, "")
	return cmd
}

// resolveFixture 构造一个含 env+user 的可解析 contextcfg 模型并解析。
func resolveFixture(t *testing.T) *contextcfg.Resolved {
	t.Helper()
	cfg := &contextcfg.Config{
		CurrentContext: "sg",
		Environments: []*contextcfg.Environment{
			{Name: "sg", ServerURL: "https://sg:18083", HubURL: "wss://sg:18083/ws", NodeID: "home", Timeout: 42, ChunkSize: 1048576},
			{Name: "office", ServerURL: "https://office:18083"},
		},
		Users: []*contextcfg.User{
			{Name: "alice", AccessKey: "ak-1", AccessKeySecret: "6162636465666768696a6b6c6d6e6f707172737475767778797a303132333435", AccessKeyID: "skey-1"},
		},
		Contexts: []*contextcfg.Context{
			{Name: "sg", Environment: "sg", User: "alice", Volume: "vol1"},
		},
	}
	r, err := contextcfg.Resolve(cfg, contextcfg.ResolveArgs{Context: "sg"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return r
}

// TestFactory_NewClient_ResolvedContext 验证：resolved 注入非 nil → NewClient
// 从 Resolved 构建（server 来自 env、凭据来自 user），不访问 cfgProvider（传 nil）。
func TestFactory_NewClient_ResolvedContext(t *testing.T) {
	t.Parallel()
	f := clientfactory.NewWithContext("test.yaml", nil, func() *contextcfg.Resolved { return resolveFixture(t) })
	svc, err := f.NewClient(contextCmd())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	if svc.ServerURL() != "https://sg:18083" {
		t.Errorf("server = %q, want https://sg:18083", svc.ServerURL())
	}
	if svc.AccessKey() != "ak-1" || svc.AccessKeySecret() != "6162636465666768696a6b6c6d6e6f707172737475767778797a303132333435" || svc.AccessKeyID() != "skey-1" {
		t.Errorf("凭据装配错: ak=%q secret=%q id=%q", svc.AccessKey(), svc.AccessKeySecret(), svc.AccessKeyID())
	}
	if svc.Volume() != "vol1" {
		t.Errorf("volume = %q, want vol1（context 覆盖字段）", svc.Volume())
	}
	if svc.MeshHubURL() != "wss://sg:18083/ws" || svc.NodeID() != "home" {
		t.Errorf("mesh 面回落错: hub=%q node=%q", svc.MeshHubURL(), svc.NodeID())
	}
}

// TestFactory_NewClient_ResolvedFlagOverrides 验证：--server/--access-key 等 flag
// 优先于 Resolved 的 env/user 值（与平铺路径同语义）。
func TestFactory_NewClient_ResolvedFlagOverrides(t *testing.T) {
	t.Parallel()
	f := clientfactory.NewWithContext("test.yaml", nil, func() *contextcfg.Resolved { return resolveFixture(t) })
	cmd := contextCmd()
	if err := cmd.Flags().Set("server", "https://override:9999"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("access-key", "ak-flag"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("access-key-secret", "6162636465666768696a6b6c6d6e6f707172737475767778797a303132333435"); err != nil {
		t.Fatal(err)
	}
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if svc.ServerURL() != "https://override:9999" {
		t.Errorf("--server 应覆盖 env server: %q", svc.ServerURL())
	}
	if svc.AccessKey() != "ak-flag" || svc.AccessKeySecret() != "6162636465666768696a6b6c6d6e6f707172737475767778797a303132333435" {
		t.Errorf("--access-key* 应覆盖 user 凭据: ak=%q", svc.AccessKey())
	}
}

// TestFactory_NewClient_ResolvedNil_FallsBackToFlat 验证：resolved 注入返回 nil
// → 回落旧平铺路径（cfgProvider 解析），双模式零破坏。
func TestFactory_NewClient_ResolvedNil_FallsBackToFlat(t *testing.T) {
	t.Parallel()
	binder := &mockCfgBinder{data: map[string]any{"server_url": "http://flat:18083"}}
	f := clientfactory.NewWithContext("test.yaml", func() clientfactory.CfgBinder { return binder }, func() *contextcfg.Resolved { return nil })
	svc, err := f.NewClient(contextCmd())
	if err != nil {
		t.Fatalf("NewClient（回落平铺）: %v", err)
	}
	if svc.ServerURL() != "http://flat:18083" {
		t.Errorf("回落平铺 server = %q, want http://flat:18083", svc.ServerURL())
	}
}

// TestResolvedToClientConfig_ZeroDefaults 验证：合成 helper 对 env 零值调优项
// 回落 pkg/client 默认（timeout 300s / chunk 4MiB），不产生零值配置。
func TestResolvedToClientConfig_ZeroDefaults(t *testing.T) {
	t.Parallel()
	r := &contextcfg.Resolved{
		Environment: &contextcfg.Environment{Name: "sg", ServerURL: "https://sg:18083"},
		User:        &contextcfg.User{Name: "alice", AccessKey: "ak-1"},
	}
	cfg := clientfactory.ResolvedToClientConfig(r)
	if cfg.ServerURL != "https://sg:18083" || cfg.AccessKey != "ak-1" {
		t.Errorf("合成字段错: %+v", cfg)
	}
	if cfg.Timeout != 300 {
		t.Errorf("timeout 零值应回落默认 300, got %d", cfg.Timeout)
	}
	if cfg.ChunkSize != 4<<20 {
		t.Errorf("chunk_size 零值应回落默认 4MiB, got %d", cfg.ChunkSize)
	}
}
