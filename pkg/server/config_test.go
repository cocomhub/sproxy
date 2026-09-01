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
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"gopkg.in/yaml.v3"
)

// compile-time interface check
var _ provider.Provider = mapProvider{}

func TestConfig_WebTunnelDefault(t *testing.T) {
	t.Parallel()
	c := Default()
	if !c.Web.Tunnel {
		t.Fatal("web.tunnel 默认应为 true")
	}
}

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

// TestConfig_Default_TCPTransportDisabled 验证 hub.transports.tcp 默认关闭
// （显式开启才生效），且默认监听地址为空（由装配点回落 127.0.0.1:18084）。
func TestConfig_Default_TCPTransportDisabled(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Hub.Transports.TCP.Enabled {
		t.Fatal("hub.transports.tcp.enabled 默认应为 false")
	}
	if cfg.Hub.Transports.TCP.Listen != "" {
		t.Fatalf("hub.transports.tcp.listen 默认应为空，got %q", cfg.Hub.Transports.TCP.Listen)
	}
}

// TestConfig_SetDefaults_TCPListen 验证 tcp 传输启用且 listen 为空时回落默认
// 127.0.0.1:18084（loopback，与 sclient relay --transport tcp 无 --hub 的默认回落一致）。
// 安全边界：默认绑定 loopback，远程可达需显式配置 listen。
func TestConfig_SetDefaults_TCPListen(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Hub.Transports.TCP.Enabled = true
	cfg.SetDefaults()
	if cfg.Hub.Transports.TCP.Listen != "127.0.0.1:18084" {
		t.Fatalf("tcp listen 默认应为 127.0.0.1:18084，got %q", cfg.Hub.Transports.TCP.Listen)
	}
	// 显式配置的 listen 应保留
	cfg2 := Default()
	cfg2.Hub.Transports.TCP.Enabled = true
	cfg2.Hub.Transports.TCP.Listen = "127.0.0.1:19000"
	cfg2.SetDefaults()
	if cfg2.Hub.Transports.TCP.Listen != "127.0.0.1:19000" {
		t.Fatalf("显式 tcp listen 应保留，got %q", cfg2.Hub.Transports.TCP.Listen)
	}
}

// TestConfig_Validate_TCPPortConflict 验证 hub TCP 中继与主 HTTP server 同端口时
// 校验失败（提前给清晰错误，而非 OS 绑定失败）。
func TestConfig_Validate_TCPPortConflict(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.Hub.Transports.TCP.Enabled = true
	cfg.Hub.Transports.TCP.Listen = ":18083" // 与默认 addr :18083 同端口
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when TCP relay port conflicts with HTTP addr")
	}
	// 不同端口应通过
	cfg2 := Default()
	cfg2.Hub.Enabled = true
	cfg2.Hub.Transports.TCP.Enabled = true
	cfg2.Hub.Transports.TCP.Listen = "127.0.0.1:18084"
	cfg2.SetDefaults()
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("different port should pass: %v", err)
	}
}

// TestConfig_Validate_HubEnabledRequiresAtLeastOneTransport 验证 hub 启用时
// ws/tcp 至少启用一种；两者皆关校验失败（旧规则只认 ws，P1 TCP 中继后放宽）。
func TestConfig_Validate_HubEnabledRequiresAtLeastOneTransport(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Hub.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when hub enabled but no transport enabled")
	}
	// 仅 tcp 启用应通过（无 WS 场景）
	cfgOnlyTCP := Default()
	cfgOnlyTCP.Hub.Enabled = true
	cfgOnlyTCP.Hub.Transports.TCP.Enabled = true
	if err := cfgOnlyTCP.Validate(); err != nil {
		t.Fatalf("hub enabled with only tcp transport should pass: %v", err)
	}
	// 仅 ws 启用应通过（向后兼容）
	cfgOnlyWS := Default()
	cfgOnlyWS.Hub.Enabled = true
	cfgOnlyWS.Hub.Transports.WS.Enabled = true
	if err := cfgOnlyWS.Validate(); err != nil {
		t.Fatalf("hub enabled with only ws transport should pass: %v", err)
	}
	// 两者都启用应通过
	cfgBoth := Default()
	cfgBoth.Hub.Enabled = true
	cfgBoth.Hub.Transports.WS.Enabled = true
	cfgBoth.Hub.Transports.TCP.Enabled = true
	if err := cfgBoth.Validate(); err != nil {
		t.Fatalf("hub enabled with ws+tcp transports should pass: %v", err)
	}
	// hub 未启用时不受影响
	cfgDisabled := Default()
	cfgDisabled.Hub.Enabled = false
	if err := cfgDisabled.Validate(); err != nil {
		t.Fatalf("hub disabled should pass regardless of transports: %v", err)
	}
}

// TestLoadFromProvider_TCPTransport 验证 YAML 解析 hub.transports.tcp 配置。
func TestLoadFromProvider_TCPTransport(t *testing.T) {
	t.Parallel()
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{
		"addr": ":18083",
		"hub": map[string]any{
			"enabled": true,
			"transports": map[string]any{
				"ws": map[string]any{"enabled": false},
				"tcp": map[string]any{
					"enabled": true,
					"listen":  "127.0.0.1:19000",
				},
			},
		},
	}})
	if err != nil {
		t.Fatalf("LoadFromProvider failed: %v", err)
	}
	if !cfg.Hub.Transports.TCP.Enabled {
		t.Fatal("expected hub.transports.tcp.enabled=true")
	}
	if cfg.Hub.Transports.TCP.Listen != "127.0.0.1:19000" {
		t.Fatalf("expected tcp listen 127.0.0.1:19000, got %q", cfg.Hub.Transports.TCP.Listen)
	}
	if cfg.Hub.Transports.WS.Enabled {
		t.Fatal("expected ws transport disabled")
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

// TestConfig_Validate_AccessKeys 覆盖 access_keys 校验（I-1/I-2）：
// Secret 长度/hex、Key 唯一、mesh_id 与 AK 内嵌 mesh 一致性。
func TestConfig_Validate_AccessKeys(t *testing.T) {
	validSecret := strings.Repeat("a", 64)

	// 合法：单 AK，无 mesh_id（默认空），Secret 64 hex。
	ok := Default()
	ok.AccessKeys = []AccessKeyConfig{{Key: "sk-prod-1234567890abcdef", Secret: validSecret}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法 access_keys 应通过: %v", err)
	}

	// Secret 非 64 hex。
	short := Default()
	short.AccessKeys = []AccessKeyConfig{{Key: "sk-prod-1234567890abcdef", Secret: "short"}}
	if err := short.Validate(); err == nil {
		t.Fatal("Secret 非 64 hex 应报错")
	}

	// Secret 非 hex。
	nonhex := Default()
	nonhex.AccessKeys = []AccessKeyConfig{{Key: "sk-prod-1234567890abcdef", Secret: strings.Repeat("g", 64)}}
	if err := nonhex.Validate(); err == nil {
		t.Fatal("Secret 非 hex 应报错")
	}

	// Key 空。
	emptyKey := Default()
	emptyKey.AccessKeys = []AccessKeyConfig{{Key: "", Secret: validSecret}}
	if err := emptyKey.Validate(); err == nil {
		t.Fatal("空 Key 应报错")
	}

	// Key 重复。
	dup := Default()
	dup.AccessKeys = []AccessKeyConfig{{Key: "sk-prod-1234567890abcdef", Secret: validSecret}, {Key: "sk-prod-1234567890abcdef", Secret: strings.Repeat("b", 64)}}
	if err := dup.Validate(); err == nil {
		t.Fatal("重复 Key 应报错")
	}

	// mesh_id 与 AK 内嵌 mesh 不一致。
	meshMismatch := Default()
	meshMismatch.AccessKeys = []AccessKeyConfig{{Key: "sk-prod-1234567890abcdef", Secret: validSecret, MeshID: "other"}}
	if err := meshMismatch.Validate(); err == nil {
		t.Fatal("mesh_id 与 AK 内嵌 mesh 不一致应报错")
	}

	// mesh_id 与 AK 内嵌 mesh 一致。
	meshOK := Default()
	meshOK.AccessKeys = []AccessKeyConfig{{Key: "sk-prod-1234567890abcdef", Secret: validSecret, MeshID: "prod"}}
	if err := meshOK.Validate(); err != nil {
		t.Fatalf("mesh_id 一致应通过: %v", err)
	}

	// AK 非 sk- 格式时 mesh_id 显式提供不校验一致性。
	nonSK := Default()
	nonSK.AccessKeys = []AccessKeyConfig{{Key: "legacy-key", Secret: validSecret, MeshID: "custom"}}
	if err := nonSK.Validate(); err != nil {
		t.Fatalf("非 sk- AK + 显式 mesh_id 应通过: %v", err)
	}
}

// TestConfig_VirtualSubnet_Default 校验 hub.virtual_subnet 默认值为 CGNAT 100.64.0.0/10，
// 空值经 SetDefaults 填充，非空值不被覆盖。
func TestConfig_VirtualSubnet_Default(t *testing.T) {
	c := Default()
	if c.Hub.VirtualSubnet != hub.DefaultVirtualSubnet {
		t.Fatalf("Default().Hub.VirtualSubnet = %q, want %q", c.Hub.VirtualSubnet, hub.DefaultVirtualSubnet)
	}
	c2 := &Config{}
	c2.SetDefaults()
	if c2.Hub.VirtualSubnet != hub.DefaultVirtualSubnet {
		t.Fatalf("SetDefaults 后 Hub.VirtualSubnet = %q, want %q", c2.Hub.VirtualSubnet, hub.DefaultVirtualSubnet)
	}
	c3 := Default()
	c3.Hub.VirtualSubnet = "10.0.0.0/8"
	c3.SetDefaults()
	if c3.Hub.VirtualSubnet != "10.0.0.0/8" {
		t.Fatalf("SetDefaults 不应覆盖非空 VirtualSubnet, got %q", c3.Hub.VirtualSubnet)
	}
}

// TestConfig_Defaults_HubDHTPersistFile 验证 hub.dht_persist_file 默认空
// （持久化缺省关闭，零行为变更）。
func TestConfig_Defaults_HubDHTPersistFile(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Hub.DHTPersistFile != "" {
		t.Fatalf("Hub.DHTPersistFile 默认应为空，got %q", cfg.Hub.DHTPersistFile)
	}
}

// TestLoadFromProvider_HubDHTPersistFile 验证 YAML 解析 hub.dht_persist_file 配置。
func TestLoadFromProvider_HubDHTPersistFile(t *testing.T) {
	t.Parallel()
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{
		"addr": ":18083",
		"hub": map[string]any{
			"enabled": true,
			"transports": map[string]any{
				"ws": map[string]any{"enabled": true},
			},
			"dht":              "kad",
			"dht_persist_file": "/tmp/kad-dht.json",
		},
	}})
	if err != nil {
		t.Fatalf("LoadFromProvider failed: %v", err)
	}
	if cfg.Hub.DHT != "kad" {
		t.Fatalf("Hub.DHT want kad, got %q", cfg.Hub.DHT)
	}
	if cfg.Hub.DHTPersistFile != "/tmp/kad-dht.json" {
		t.Fatalf("Hub.DHTPersistFile want /tmp/kad-dht.json, got %q", cfg.Hub.DHTPersistFile)
	}
}

// TestConfig_StorageRoot 验证 storage_root 回退与 owner_quotas 默认值逻辑（任务 4）。
// StorageRoot() 字段非空优先，否则回退 UploadsDir（兼容旧配置）；OwnerQuotaFor 显式 owner > "*" > 0。
func TestConfig_StorageRoot(t *testing.T) {
	c := Default()
	c.UploadsDir = "./storage"
	if c.StorageRoot() != "./storage" {
		t.Fatalf("StorageRoot=%q", c.StorageRoot())
	}
	// OwnerQuotaFor：未配置返回默认值
	if got := c.OwnerQuotaFor("alice"); got != 0 {
		t.Fatalf("OwnerQuotaFor(默认)=%d want 0", got)
	}
	c.OwnerQuotas = map[string]int64{"*": 5 << 30, "alice": 10 << 30}
	if got := c.OwnerQuotaFor("alice"); got != 10<<30 {
		t.Fatalf("OwnerQuotaFor(alice)=%d", got)
	}
	if got := c.OwnerQuotaFor("bob"); got != 5<<30 {
		t.Fatalf("OwnerQuotaFor(bob 默认*)=%d", got)
	}
}

// TestLoadFromProvider_StorageRootOwnerQuotas 验证 YAML 解析 storage_root/owner_quotas 配置
// （owner_quotas 值为字节数整数；"*" 为默认值）。
func TestLoadFromProvider_StorageRootOwnerQuotas(t *testing.T) {
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{
		"storage_root": "./storage",
		"owner_quotas": map[string]any{"*": int64(5 << 30), "alice": int64(10 << 30)},
	}})
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.StorageRoot() != "./storage" {
		t.Fatalf("StorageRoot()=%q want ./storage", cfg.StorageRoot())
	}
	if got := cfg.OwnerQuotaFor("alice"); got != 10<<30 {
		t.Fatalf("OwnerQuotaFor(alice)=%d want %d", got, 10<<30)
	}
	if got := cfg.OwnerQuotaFor("bob"); got != 5<<30 {
		t.Fatalf("OwnerQuotaFor(bob)=%d want %d（回退 * 默认）", got, 5<<30)
	}
}

// TestLoadFromProvider_UploadsDirFallback 验证仅配置 uploads_dir（旧配置）时 StorageRoot() 回退
// uploads_dir，保证向后兼容。
func TestLoadFromProvider_UploadsDirFallback(t *testing.T) {
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{"uploads_dir": "/old/uploads"}})
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.StorageRoot() != "/old/uploads" {
		t.Fatalf("StorageRoot()=%q want /old/uploads（回退 uploads_dir）", cfg.StorageRoot())
	}
	// 显式 storage_root 优先于 uploads_dir
	cfg2, err := LoadFromProvider(mapProvider{m: map[string]any{
		"uploads_dir":  "/old/uploads",
		"storage_root": "/new/storage",
	}})
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg2.StorageRoot() != "/new/storage" {
		t.Fatalf("StorageRoot()=%q want /new/storage（storage_root 优先）", cfg2.StorageRoot())
	}
}

// TestConfig_VirtualSubnet_Validate 校验 hub.virtual_subnet 校验：非法 CIDR 拒绝、
// IPv6 拒绝、合法 IPv4 通过。
func TestConfig_VirtualSubnet_Validate(t *testing.T) {
	valid := Default()
	valid.Hub.VirtualSubnet = "10.0.0.0/8"
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法 IPv4 子网应通过: %v", err)
	}
	invalid := Default()
	invalid.Hub.VirtualSubnet = "not-a-cidr"
	if err := invalid.Validate(); err == nil {
		t.Fatal("非法 CIDR 应被 Validate 拒绝")
	}
	ipv6 := Default()
	ipv6.Hub.VirtualSubnet = "fd00::/8"
	if err := ipv6.Validate(); err == nil {
		t.Fatal("IPv6 子网应被 Validate 拒绝（虚拟 IP 分配仅支持 IPv4）")
	}
}
