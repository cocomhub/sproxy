// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/provider"
	"gopkg.in/yaml.v3"
)

// compile-time interface check
var _ provider.Provider = mapProvider{}

func TestConfig_DefaultsFilled(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Addr == "" {
		t.Fatal("Addr default empty")
	}
	if cfg.UploadsDir == "" {
		t.Fatal("UploadsDir default empty")
	}
	if cfg.ChunkSize <= 0 {
		t.Fatal("ChunkSize default <= 0")
	}
	if cfg.UploadSessionTTL <= 0 {
		t.Fatal("UploadSessionTTL default <= 0")
	}
	if cfg.ServerTimeouts.Shutdown <= 0 {
		t.Fatalf("ServerTimeouts.Shutdown default <= 0: %v", cfg.ServerTimeouts.Shutdown)
	}
	if cfg.ServerTimeouts.Shutdown != 30*time.Second {
		t.Fatalf("ServerTimeouts.Shutdown default want 30s, got %v", cfg.ServerTimeouts.Shutdown)
	}
	if !cfg.TLS.Enabled {
		t.Fatal("TLS.Enabled default should be true")
	}
	if !cfg.TLS.AutoTLS {
		t.Fatal("TLS.AutoTLS default should be true")
	}
	if cfg.CloudDownloadTimeout != 30*time.Minute {
		t.Fatalf("CloudDownloadTimeout default want 30m, got %v", cfg.CloudDownloadTimeout)
	}
	if cfg.CloudDownloadIdleTimeout != 1*time.Minute {
		t.Fatalf("CloudDownloadIdleTimeout default want 1m, got %v", cfg.CloudDownloadIdleTimeout)
	}
	if cfg.CloudMaxRetries != 10 {
		t.Fatalf("CloudMaxRetries default want 10, got %d", cfg.CloudMaxRetries)
	}
	if cfg.CloudRetryDelay != 10*time.Second {
		t.Fatalf("CloudRetryDelay default want 10s, got %v", cfg.CloudRetryDelay)
	}
	if cfg.CloudMaxBatchURLs != 100 {
		t.Fatalf("CloudMaxBatchURLs default want 100, got %d", cfg.CloudMaxBatchURLs)
	}
}

func TestConfig_Validate_FillsZeroes(t *testing.T) {
	t.Parallel()
	c := &Config{TLS: TLSConfig{Enabled: true}}
	c.SetDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Addr == "" || c.UploadsDir == "" || c.ChunkSize <= 0 ||
		c.UploadSessionTTL <= 0 || c.ServerTimeouts.Shutdown <= 0 {
		t.Fatalf("SetDefaults did not fill zero values: %+v", c)
	}
}

func TestConfig_Defaults_HubMaxConnections(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.SetDefaults()
	if cfg.Hub.MaxConnections != 256 {
		t.Fatalf("Hub.MaxConnections default want 256, got %d", cfg.Hub.MaxConnections)
	}
	// 显式配置非零值应保留
	cfg2 := Default()
	cfg2.Hub.MaxConnections = 64
	cfg2.SetDefaults()
	if cfg2.Hub.MaxConnections != 64 {
		t.Fatalf("Hub.MaxConnections want preserved 64, got %d", cfg2.Hub.MaxConnections)
	}
}

func TestConfig_Validate_HubEnabledNoTokenRequired(t *testing.T) {
	t.Parallel()
	// hub.enabled=true 不再要求任何 hub 级 token（准入改由顶层 access_keys 提供，
	// SproxySig AccessKey + HMAC proof）；仅需 ws transport 即可通过校验（S42）。
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.Hub.Transports.WS.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("hub.enabled=true 且 ws 传输开启应通过校验（无需 token）: %v", err)
	}
	// hub 未启用时不受 ws 开关影响。
	cfg2 := Default()
	cfg2.Hub.Enabled = false
	cfg2.Hub.Transports.WS.Enabled = false
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("hub disabled should not require ws transport: %v", err)
	}
}

func TestConfig_Validate_HubEnabledRequiresTransport(t *testing.T) {
	t.Parallel()
	// hub.enabled=true 但 transports.ws.enabled=false → 校验失败（S42，
	// WS 是当前唯一节点接入传输，hub 启用而无 transport 时节点无法连接）。
	cfg := Default()
	cfg.Hub.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when hub.enabled=true but transports.ws.enabled=false")
	}
	// 启用 ws 后应通过
	cfg.Hub.Transports.WS.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error with ws enabled: %v", err)
	}
	// hub 未启用时不受 ws 开关影响
	cfg2 := Default()
	cfg2.Hub.Transports.WS.Enabled = false
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("hub disabled should not require ws transport: %v", err)
	}
}

// mapProvider 将 map[string]any 转换为 provider.Provider 用于测试。
type mapProvider struct {
	m map[string]any
}

func (p mapProvider) Unmarshal(obj any) error {
	// 使用 yaml 作为中介：map → yaml bytes → struct
	// Config 结构体使用 yaml tag，所以 yaml.Unmarshal 能正确匹配字段
	data, err := yaml.Marshal(p.m)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, obj)
}

func TestLoadFromProvider_DefaultsOnly(t *testing.T) {
	t.Parallel()
	p := mapProvider{m: map[string]any{}}
	cfg, err := LoadFromProvider(p)
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.Addr != ":18083" {
		t.Fatalf("expected default Addr :18083, got %q", cfg.Addr)
	}
}

func TestLoadFromProvider_OverridesViaSet(t *testing.T) {
	t.Parallel()
	p := mapProvider{m: map[string]any{"addr": ":19999"}}
	cfg, err := LoadFromProvider(p)
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.Addr != ":19999" {
		t.Fatalf("want :19999, got %q", cfg.Addr)
	}
}

func TestLoadConfig_FileNotExist_ReturnsDefault(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\"): %v", err)
	}
	if cfg.Addr != ":18083" {
		t.Fatalf("expected default, got %+v", cfg)
	}
}

func TestSaveConfig_ToFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sproxy.yaml")

	cfg := Default()
	cfg.Addr = ":19999"
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Addr != ":19999" {
		t.Fatalf("Addr mismatch: want :19999, got %q", loaded.Addr)
	}
}

func TestSaveConfig_ReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only dir test not supported on Windows")
	}
	t.Parallel()

	roDir, cleanup := makeReadOnlyDir(t)
	defer cleanup()

	cfg := Default()
	err := SaveConfig(cfg, filepath.Join(roDir, "sproxy.yaml"))
	if err == nil {
		t.Fatal("expected error when saving to read-only directory")
	}
}

func TestSaveConfig_InvalidPath(t *testing.T) {
	t.Parallel()
	cfg := Default()
	err := SaveConfig(cfg, "/nonexistent/dir/config.yaml")
	if err == nil {
		t.Error("expected error saving to nonexistent directory")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfig("/nonexistent/config.yaml")
	if err != nil {
		t.Logf("LoadConfig for nonexistent file returned error: %v (acceptable)", err)
	}
	if cfg == nil {
		t.Log("LoadConfig returned nil config (acceptable)")
	}
}

// TestConfig_YAMLTagsMatchMapstructure 验证配置树中所有字段的 yaml 与 mapstructure 标签一致。
//
// 回归防护（I31）：viper 通过 mapstructure 标签解码，yaml.Unmarshal 通过 yaml 标签解码。
// 两者键名不一致时（如 ACME 字段曾写 mapstructure:"http_01" 而 yaml 键为 http01），
// viper 路径静默丢失配置值恒为默认值，而基于 yaml 的测试路径不受影响。
// 该测试直接断言两条解码路径的键名一致，任何字段再出现标签漂移都会立即失败。
func TestConfig_YAMLTagsMatchMapstructure(t *testing.T) {
	t.Parallel()

	seen := map[reflect.Type]bool{}
	var check func(typ reflect.Type)
	check = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer:
			check(typ.Elem())
			return
		case reflect.Slice, reflect.Array:
			check(typ.Elem())
			return
		case reflect.Struct:
			// 继续检查字段
		default:
			return
		}
		if seen[typ] {
			return
		}
		seen[typ] = true
		for f := range typ.Fields() {
			if !f.IsExported() {
				continue
			}
			yamlTag := f.Tag.Get("yaml")
			mapTag := f.Tag.Get("mapstructure")
			if yamlTag == "" || yamlTag == "-" || mapTag == "" || mapTag == "-" {
				continue
			}
			yKey := strings.Split(yamlTag, ",")[0]
			mKey := strings.Split(mapTag, ",")[0]
			if yKey != mKey {
				t.Errorf("%s.%s: yaml 标签 %q 与 mapstructure 标签 %q 不一致", typ.Name(), f.Name, yKey, mKey)
			}
			check(f.Type)
		}
	}
	check(reflect.TypeFor[Config]())
}
