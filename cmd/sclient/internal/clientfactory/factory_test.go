// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

func TestFactory_Constructor(t *testing.T) {
	// New 和 NewMock 返回 Factory 接口 — 编译期检查构造函数签名正确
	_ = clientfactory.New("", nil)
	_ = clientfactory.NewMock(nil, nil)
}

func TestFactoryLazy_GetProvider(t *testing.T) {
	// 验证延迟获取：init 时传入 nil provider，PersistentPreRunE 后提供真实值
	var called bool
	_ = clientfactory.New("", func() clientfactory.CfgBinder {
		called = true
		return nil
	})
	if called {
		t.Error("CfgBinder function should not be called during New()")
	}
}

func TestMockFactory_NilService(t *testing.T) {
	cmd := &cobra.Command{}
	factory := clientfactory.NewMock(nil, nil)
	svc, err := factory.NewClient(cmd)
	if err != nil {
		t.Fatalf("NewClient should not error: %v", err)
	}
	if svc != nil {
		t.Error("NewClient should return nil service as-is")
	}
}

// mockCfgBinder 实现 CfgBinder 接口，用于生产 Factory 测试。
type mockCfgBinder struct {
	data map[string]any
}

func (m *mockCfgBinder) BindPFlag(key string, flag *pflag.Flag) {}

func (m *mockCfgBinder) Unmarshal(obj any) error {
	// 使用 yaml 作为中介：map → yaml bytes → struct。
	// Config 结构体使用 yaml/mapstructure 蛇形键（如 peer_fingerprints、hub_url），
	// json.Unmarshal 无法匹配下划线键，故此处用 yaml。
	data, err := yaml.Marshal(m.data)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, obj)
}

func TestFactory_NewClient_NilProvider(t *testing.T) {
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return nil })
	cmd := &cobra.Command{}
	svc, err := f.NewClient(cmd)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
	if svc != nil {
		t.Fatal("expected nil service when provider is nil")
	}
}

func TestFactory_NewClient_WithConfig(t *testing.T) {
	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

func TestFactory_NewClient_FlagOverridesServer(t *testing.T) {
	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://original:8080",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("auth-token", "", "")
	cmd.Flags().Int64("chunk-size", 0, "")
	if err := cmd.Flags().Set("server", "http://override:8080"); err != nil {
		t.Fatal(err)
	}
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

func TestFactory_NewClient_WithAuthToken(t *testing.T) {
	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
			"auth_token": "test-token",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("auth-token", "", "")
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

// setXDGConfigHome 在测试中替换 xdg.ConfigHome 为临时目录，并注册恢复。
// 注意：本测试不使用 t.Parallel()，避免与其他读取 xdg.ConfigHome 的测试并发冲突。
func setXDGConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := xdg.ConfigHome
	xdg.ConfigHome = dir
	t.Cleanup(func() { xdg.ConfigHome = old })
	return dir
}

// writeCorruptIdentity 在 XDG 配置目录写入损坏的身份文件。
func writeCorruptIdentity(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "sproxy", "identity.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("garbage-not-json"), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestFactory_NewClient_CorruptIdentity_NonXferStillWorks 验证 M-1 懒加载：
// 身份文件损坏不应导致 upload/download/list 等非 xfer 命令全部不可用。
func TestFactory_NewClient_CorruptIdentity_NonXferStillWorks(t *testing.T) {
	dir := setXDGConfigHome(t)
	writeCorruptIdentity(t, dir)

	binder := &mockCfgBinder{
		data: map[string]any{"server_url": "http://127.0.0.1:18083"},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("非 xfer 命令不应因身份文件损坏而失败（M-1 懒加载）: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

// TestFactory_NewClient_Xfer_CorruptIdentity_ErrorsWithRecovery 验证 M-1：
// xfer 隧道模式消费身份，损坏时 fail-closed 报错并给出恢复路径（sclient identity generate --force）。
func TestFactory_NewClient_Xfer_CorruptIdentity_ErrorsWithRecovery(t *testing.T) {
	dir := setXDGConfigHome(t)
	writeCorruptIdentity(t, dir)

	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
			"hub_url":    "127.0.0.1:9999",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	if err := cmd.Flags().Set("xfer", "tcp"); err != nil {
		t.Fatal(err)
	}
	_, err := f.NewClient(cmd)
	if err == nil {
		t.Fatal("xfer 模式下身份文件损坏应 fail-closed 报错")
	}
	if !strings.Contains(err.Error(), "identity generate --force") {
		t.Fatalf("错误应给出恢复路径（sclient identity generate --force）, 实际: %v", err)
	}
}

// TestFactory_NewClient_Xfer_IdentityAndPinWired 验证 M-1/H-1：
// xfer 模式配置身份与 peer_fingerprints 时，生成的客户端携带 pinning 选项（可经 TunnelDo 校验）。
func TestFactory_NewClient_Xfer_IdentityAndPinWired(t *testing.T) {
	id, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir := setXDGConfigHome(t)
	if err = tunnel.SaveIdentity(id, filepath.Join(dir, "sproxy", "identity.json")); err != nil {
		t.Fatal(err)
	}

	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url":        "http://127.0.0.1:18083",
			"hub_url":           "127.0.0.1:9999",
			"access_key":        "sk-00000000000000000000000000000000",
			"access_key_secret": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"peer_fingerprints": []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	if err = cmd.Flags().Set("xfer", "tcp"); err != nil {
		t.Fatal(err)
	}
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("xfer 模式配置身份 + pin 应成功: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	// pinning 选项已接线：client 内部 identity / peerFingerprints 非空（H-1 端到端由
	// cmd/sclient 的 TestTunnelCmd_Xfer_Pinning_Enforced 验证真实传输）。
	if svc.Identity() == nil {
		t.Fatal("xfer 模式应加载本端身份")
	}
	if len(svc.PeerFingerprints()) == 0 {
		t.Fatal("xfer 模式应配置对端指纹 pin")
	}
}

// TestFactory_NewClient_Xfer_NoKey_FailsClosed 验证：xfer 隧道模式配置了身份/peer_fingerprints
// 但未配置 access_key_secret（隧道 key 为 nil → 握手不执行 → pinning 静默不生效）时
// fail-closed 报错（而非仅 Warn），避免安全机制被无声绕过。
func TestFactory_NewClient_Xfer_NoKey_FailsClosed(t *testing.T) {
	id, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir := setXDGConfigHome(t)
	if err = tunnel.SaveIdentity(id, filepath.Join(dir, "sproxy", "identity.json")); err != nil {
		t.Fatal(err)
	}

	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
			"hub_url":    "127.0.0.1:9999",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	if err = cmd.Flags().Set("xfer", "tcp"); err != nil {
		t.Fatal(err)
	}
	_, err = f.NewClient(cmd)
	if err == nil {
		t.Fatal("xfer 配置身份但缺 access_key_secret 应 fail-closed 报错")
	}
	if !strings.Contains(err.Error(), "access_key_secret") {
		t.Fatalf("错误应指明需配置 access_key_secret, 实际: %v", err)
	}
}

// TestFactory_NewClient_Xfer_NoKeyNoPin_Succeeds 验证：xfer 模式无身份、无 peer_fingerprints、
// 无 access_key_secret 时仍正常创建客户端（无 pinning 预期，向后兼容）。
func TestFactory_NewClient_Xfer_NoKeyNoPin_Succeeds(t *testing.T) {
	setXDGConfigHome(t) // 空 XDG：无身份文件

	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
			"hub_url":    "127.0.0.1:9999",
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	if err := cmd.Flags().Set("xfer", "tcp"); err != nil {
		t.Fatal(err)
	}
	svc, err := f.NewClient(cmd)
	if err != nil {
		t.Fatalf("xfer 无身份/无 pin/无 key 应正常创建客户端: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

// TestFactory_NewClient_PeerFingerprintsNonXfer_FailsClosed 验证：配置了 peer_fingerprints
// 但命令不走 xfer 隧道时 fail-closed 报错（而非仅 Warn），防止用户误以为受 pinning 保护。
func TestFactory_NewClient_PeerFingerprintsNonXfer_FailsClosed(t *testing.T) {
	binder := &mockCfgBinder{
		data: map[string]any{
			"server_url": "http://127.0.0.1:18083",
			"peer_fingerprints": []string{
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
	}
	f := clientfactory.New("test.yaml", func() clientfactory.CfgBinder { return binder })
	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	_, err := f.NewClient(cmd)
	if err == nil {
		t.Fatal("非 xfer + peer_fingerprints 应 fail-closed 报错")
	}
	if !strings.Contains(err.Error(), "xfer") {
		t.Fatalf("错误应指引使用 xfer 隧道, 实际: %v", err)
	}
}
