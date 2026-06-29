// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxycfg_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/provider"
)

func TestNew_NoConfigFile(t *testing.T) {
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
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
addr: ":19000"
uploads_dir: "/tmp/uploads"
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
		Addr       string `mapstructure:"addr"`
		UploadsDir string `mapstructure:"uploads_dir"`
		LogLevel   string `mapstructure:"log_level"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if cfg.Addr != ":19000" {
		t.Errorf("addr = %q, want %q", cfg.Addr, ":19000")
	}
	if cfg.UploadsDir != "/tmp/uploads" {
		t.Errorf("uploads_dir = %q, want %q", cfg.UploadsDir, "/tmp/uploads")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "debug")
	}
}

func TestRefresh(t *testing.T) {
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
	// 运行时验证 ViperProvider 实现了 Provider 和 Refresher 接口
	vp := sproxycfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))

	var _ provider.Provider = vp
	var _ provider.Refresher = vp
	// 如果能编译到这里，说明接口满足
}
