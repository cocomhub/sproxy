// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxycfg_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/server"
)

func TestNew_NoConfigFile(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	vp := sproxycfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if vp == nil {
		t.Fatal("New() 文件不存在时应返回非 nil ViperProvider")
	}
	// 不应 panic，Unmarshal 到空结构体也不应报错
	var cfg struct {
		Addr string `mapstructure:"addr"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 空配置不应报错: %v", err)
	}
}

func TestNew_WithConfigFile(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
addr: ":19000"
storage_root: "/tmp/uploads"
log_level: "debug"
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	vp := sproxycfg.New(cfgPath)
	if vp == nil {
		t.Fatal("New() 有效配置文件时应返回非 nil ViperProvider")
	}

	var cfg struct {
		Addr        string `mapstructure:"addr"`
		StorageRoot string `mapstructure:"storage_root"`
		LogLevel    string `mapstructure:"log_level"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if cfg.Addr != ":19000" {
		t.Errorf("addr = %q, want %q", cfg.Addr, ":19000")
	}
	if cfg.StorageRoot != "/tmp/uploads" {
		t.Errorf("storage_root = %q, want %q", cfg.StorageRoot, "/tmp/uploads")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "debug")
	}
}

// TestViperUnmarshal_ByteSizeHumanReadable 验证 viper 路径（真实 YAML 文件）下
// owner_quotas/bucket_limits/vol_capacity 支持人类可读大小（ByteSize hook 生效）。
func TestViperUnmarshal_ByteSizeHumanReadable(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
storage_root: "./storage"
owner_quotas:
  "*": "5GiB"
  alice: "2GB"
  bob: "1.5MiB"
  anonymous: 1073741824
bucket_limits:
  user/videos/hd: "10MiB"
volumes:
  - name: disk1
    root: /mnt/d1
    vol_capacity: "500GB"
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	vp := sproxycfg.New(cfgPath)
	var cfg server.Config
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if got := cfg.OwnerQuotaFor("*"); got != 5<<30 {
		t.Errorf("OwnerQuotaFor(*)=%d want %d (5GiB)", got, 5<<30)
	}
	if got := cfg.OwnerQuotaFor("alice"); got != 2*1000*1000*1000 {
		t.Errorf("OwnerQuotaFor(alice)=%d want %d (2GB)", got, 2*1000*1000*1000)
	}
	if got := cfg.OwnerQuotaFor("bob"); got != int64(1.5*1024*1024) {
		t.Errorf("OwnerQuotaFor(bob)=%d want %d (1.5MiB)", got, int64(1.5*1024*1024))
	}
	if got := cfg.OwnerQuotaFor("anonymous"); got != 1073741824 {
		t.Errorf("OwnerQuotaFor(anonymous)=%d want 1073741824（纯数字）", got)
	}
	if got := cfg.BucketLimits["user/videos/hd"]; got != 10<<20 {
		t.Errorf("BucketLimits[user/videos/hd]=%d want %d (10MiB)", got, 10<<20)
	}
	if len(cfg.Volumes) != 1 || int64(cfg.Volumes[0].VolCapacity) != 500*1000*1000*1000 {
		t.Errorf("Volumes[0].VolCapacity=%d want %d (500GB)", int64(cfg.Volumes[0].VolCapacity), 500*1000*1000*1000)
	}
}

// TestViperUnmarshal_ByteSizeBadValue 验证 viper 路径非法人类可读值解码失败（fail-closed）。
func TestViperUnmarshal_ByteSizeBadValue(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
owner_quotas:
  alice: "5XB"
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	vp := sproxycfg.New(cfgPath)
	var cfg server.Config
	if err := vp.Unmarshal(&cfg); err == nil {
		t.Fatal("非法人类可读大小应解码失败")
	}
}

func TestRefresh(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// 写入初始配置
	yamlInitial := `addr: ":18080"`
	if err := os.WriteFile(cfgPath, []byte(yamlInitial), 0644); err != nil {
		t.Fatal(err)
	}

	vp := sproxycfg.New(cfgPath)

	// 修改配置文件
	yamlUpdated := `addr: ":19090"`
	if err := os.WriteFile(cfgPath, []byte(yamlUpdated), 0644); err != nil {
		t.Fatal(err)
	}

	// Refresh 重读配置
	if err := vp.Refresh(); err != nil {
		t.Fatalf("Refresh 应成功: %v", err)
	}

	var cfg struct {
		Addr string `mapstructure:"addr"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if cfg.Addr != ":19090" {
		t.Errorf("Refresh 后 addr = %q, want %q", cfg.Addr, ":19090")
	}
}

func TestSet_and_Unmarshal(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	vp := sproxycfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))

	// 通过 Set 设置配置值
	vp.Set("addr", ":18888")
	vp.Set("log_level", "warn")

	var cfg struct {
		Addr     string `mapstructure:"addr"`
		LogLevel string `mapstructure:"log_level"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if cfg.Addr != ":18888" {
		t.Errorf("addr = %q, want %q", cfg.Addr, ":18888")
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "warn")
	}
}

func TestInterfaceCheck(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	// 运行时验证 ViperProvider 实现了 Provider 和 Refresher 接口
	vp := sproxycfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))

	var _ provider.Provider = vp
	var _ provider.Refresher = vp
	// 如果能编译到这里，说明接口满足
}

// TestViperUnmarshal_ACMEMapstructure 验证 viper 路径下 ACME 配置的 mapstructure 标签
// 与 yaml 键名一致（I31 回归）。此前 ACMEConfig.HTTP01/HTTP01Port 的 mapstructure 标签
// 写成 http_01/http_01_port，与 yaml 键 http01/http01_port 不一致，导致 viper 解码
// 恒为默认值（HTTP01 恒 false、HTTP01Port 恒空串）。
//
// 必须使用真实 viper（本包）而非 yaml.Unmarshal：pkg/server 的 yaml 路径不受标签漂移影响，
// 只有 viper 的 mapstructure 路径会静默丢值。
func TestViperUnmarshal_ACMEMapstructure(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
tls:
  acme:
    http01: true
    http01_port: 80
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	vp := sproxycfg.New(cfgPath)
	cfg, err := server.LoadFromProvider(vp)
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if !cfg.TLS.ACME.HTTP01 {
		t.Error("ACME.HTTP01 viper 解码失败：http01: true 未被还原")
	}
	if cfg.TLS.ACME.HTTP01Port != "80" {
		t.Errorf("ACME.HTTP01Port viper 解码失败：want %q, got %q", "80", cfg.TLS.ACME.HTTP01Port)
	}
}
