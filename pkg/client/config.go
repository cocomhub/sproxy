// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"gopkg.in/yaml.v3"
)

// Config 是 sclient 的配置文件结构。
type Config struct {
	ServerURL    string `yaml:"server_url" mapstructure:"server_url"`
	Timeout      int    `yaml:"timeout" mapstructure:"timeout"`
	ChunkSize    int64  `yaml:"chunk_size" mapstructure:"chunk_size"`
	MaxChunkSize int64  `yaml:"max_chunk_size" mapstructure:"max_chunk_size"`
	// AccessKey / AccessKeySecret 是 SproxySig 请求签名认证（替代旧 auth_token）。
	// Secret 只存本端计算签名，永不上线；服务端凭据 Ring 登记了该 AK/SK 时必填。
	AccessKey       string `yaml:"access_key" mapstructure:"access_key"`
	AccessKeySecret string `yaml:"access_key_secret" mapstructure:"access_key_secret"`
	// AccessKeyID 是 SproxySig 的 SK 条目 ID（entryID，可选）。服务端凭据 Ring
	// 多 SK 共存时，签发请求携带 sk=<id> 使服务端精确取条目；`trust renew` 成功后
	// 自动回填为新的 sk_id。为空则 entryID 空段（服务端按 AK 试签定位）。
	AccessKeyID            string `yaml:"access_key_id" mapstructure:"access_key_id"`
	AllowTransportFallback bool   `yaml:"allow_transport_fallback" mapstructure:"allow_transport_fallback"`
	// HubURL 是 mesh/relay/p2p 共用的 hub 地址（http(s):// 或 ws(s)://，接受带 /ws 路径）。
	// 为空时各命令按自身语义回落（mesh connect → server_url，p2p → 报错，relay start → 本地默认）。
	HubURL string `yaml:"hub_url" mapstructure:"hub_url"`
	// NodeID 是本节点默认 ID（mesh/p2p/relay 的信令来源与寻址目标；为空回落主机名）。
	NodeID string `yaml:"node_id" mapstructure:"node_id"`
	// PeerFingerprints 是对端身份指纹 pinning 列表（P1 身份 pinning）。
	// 配置后 xfer 隧道握手时校验对端身份指纹，不匹配或对端未提供身份时 fail-closed 拒绝。
	// 指纹格式：64 hex 或 "sha256:<64 hex>"。本端身份由 `sclient identity generate` 生成。
	PeerFingerprints []string `yaml:"peer_fingerprints" mapstructure:"peer_fingerprints"`
	// XferCAFile 是 xfer tcp+tls 传输的受信 CA 文件路径（PEM）。为空时用系统根池严格校验
	// （服务端为自签证书时需配置此项或 XferInsecure，否则握手报 x509 unknown-authority）。
	XferCAFile string `yaml:"xfer_ca_file" mapstructure:"xfer_ca_file"`
	// XferInsecure 跳过 xfer tcp+tls 传输的证书校验（仅限 loopback hub；远程 + insecure
	// fail-closed 拒绝，对齐 federation Config.Validate）。与 XferCAFile 互斥。
	XferInsecure bool `yaml:"xfer_insecure" mapstructure:"xfer_insecure"`
}

func DefaultConfig() *Config {
	return &Config{
		ServerURL:    "https://127.0.0.1:18083",
		Timeout:      300,
		ChunkSize:    size.DefaultChunkSize,    // 4 MiB
		MaxChunkSize: size.DefaultMaxChunkSize, // 64 MiB
	}
}

// SetDefaults 设置零值字段为默认值（副作用）。调用 Validate 前必须调用此方法。
func (c *Config) SetDefaults() {
	if c == nil {
		return
	}
	if c.ServerURL == "" {
		c.ServerURL = "https://127.0.0.1:18083"
	}
	if c.Timeout <= 0 {
		c.Timeout = 300
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = size.DefaultChunkSize
	}
	if c.MaxChunkSize <= 0 {
		c.MaxChunkSize = size.DefaultMaxChunkSize
	}
}

// Validate 校验配置合理性，不修改字段值。
// 设置默认值请在调用 Validate 前先调用 SetDefaults()。
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if !isValidServerURL(c.ServerURL) {
		return fmt.Errorf("server_url 无效: %s", c.ServerURL)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("timeout 必须大于 0")
	}
	if c.ChunkSize <= 0 {
		return fmt.Errorf("chunk_size 必须大于 0")
	}
	// m-4：peer_fingerprints 配置在加载阶段响亮校验（而非握手时静默跳过非法指纹）。
	for _, fp := range c.PeerFingerprints {
		if _, err := tunnel.ParseFingerprint(fp); err != nil {
			return fmt.Errorf("peer_fingerprints 含非法指纹 %q: %w", fp, err)
		}
	}
	return nil
}

// LoadFromProvider 从 provider.Provider 解码配置，设置默认值并校验。
func LoadFromProvider(p provider.Provider) (*Config, error) {
	cfg := DefaultConfig()
	if err := p.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解码失败: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // 返回默认配置，不创建文件
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	if len(data) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func SaveConfig(cfg *Config, path string) error {
	if containsPathTraversal(path) {
		return fmt.Errorf("配置路径包含非法路径穿越: %s", path)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}

// isValidServerURL 校验 server URL 格式是否合法。
func isValidServerURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func HandleConfigShow(cfg *Config, w io.Writer) {
	if cfg == nil {
		return
	}
	fmt.Fprintf(w, "ServerURL:     %s\n", cfg.ServerURL)
	fmt.Fprintf(w, "Timeout:       %d\n", cfg.Timeout)
	// AccessKeySecret 属凭据（S49）：全掩，不泄露任何 hex（M2——不再打前缀）。
	maskedSecret := "****"
	if cfg.AccessKeySecret == "" {
		maskedSecret = ""
	}
	fmt.Fprintf(w, "AccessKey:     %s\n", cfg.AccessKey)
	fmt.Fprintf(w, "AccessKeySecret: %s\n", maskedSecret)
	if cfg.AccessKeyID != "" {
		fmt.Fprintf(w, "AccessKeyID:   %s\n", cfg.AccessKeyID)
	}
	fmt.Fprintf(w, "ChunkSize:     %d\n", cfg.ChunkSize)
	fmt.Fprintf(w, "MaxChunkSize:  %d\n", cfg.MaxChunkSize)
	fmt.Fprintf(w, "AllowTransportFallback: %v\n", cfg.AllowTransportFallback)
	if cfg.HubURL != "" {
		fmt.Fprintf(w, "HubURL:        %s\n", cfg.HubURL)
	}
	if cfg.NodeID != "" {
		fmt.Fprintf(w, "NodeID:        %s\n", cfg.NodeID)
	}
	if len(cfg.PeerFingerprints) > 0 {
		fmt.Fprintf(w, "PeerFingerprints: %s\n", strings.Join(cfg.PeerFingerprints, ","))
	}
	if cfg.XferCAFile != "" {
		fmt.Fprintf(w, "XferCAFile:      %s\n", cfg.XferCAFile)
	}
	if cfg.XferInsecure {
		fmt.Fprintf(w, "XferInsecure:    %v\n", cfg.XferInsecure)
	}
}

// ApplyConfigSet 在内存中更新配置，不写文件。返回更新后的配置和错误。
func ApplyConfigSet(cfg *Config, key, value string) error {
	switch key {
	case "server_url":
		if !isValidServerURL(value) {
			return fmt.Errorf("无效的服务器地址: %s", value)
		}
		cfg.ServerURL = value
	case "access_key":
		cfg.AccessKey = value
	case "access_key_secret":
		cfg.AccessKeySecret = value
	case "access_key_id":
		cfg.AccessKeyID = value
	case "timeout":
		if timeout, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("无效的超时值: %w", err)
		} else {
			cfg.Timeout = timeout
		}
	case "chunk_size":
		chunkSize, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("无效的分块大小: %w", err)
		}
		cfg.ChunkSize = chunkSize
	case "max_chunk_size":
		maxChunkSize, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("无效的最大分块大小: %w", err)
		}
		cfg.MaxChunkSize = maxChunkSize
	case "hub_url":
		if value != "" {
			u, perr := url.Parse(value)
			if perr != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("无效的 hub 地址: %s", value)
			}
		}
		cfg.HubURL = value
	case "node_id":
		if strings.ContainsAny(value, " \t\r\n") {
			return fmt.Errorf("node_id 不能包含空白字符: %s", value)
		}
		cfg.NodeID = value
	case "peer_fingerprints":
		fps := []string{}
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, err := tunnel.ParseFingerprint(part); err != nil {
				return fmt.Errorf("无效的对端指纹: %s（应为 64 hex 或 sha256:<64 hex>）", part)
			}
			fps = append(fps, part)
		}
		cfg.PeerFingerprints = fps
	case "xfer_ca_file":
		cfg.XferCAFile = value
	case "xfer_insecure":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("无效的 xfer_insecure: %w（应为 true/false）", err)
		}
		cfg.XferInsecure = b
	default:
		return fmt.Errorf("未知配置键: %s", key)
	}
	return nil
}

// HandleConfigSet 更新配置并写入文件。
func HandleConfigSet(cfg *Config, configPath, key, value string) error {
	if err := ApplyConfigSet(cfg, key, value); err != nil {
		return err
	}
	return SaveConfig(cfg, configPath)
}

// ConfigResponse 是 GET /api/config 的响应结构体。
type ConfigResponse struct {
	LogLevel           string `json:"log_level"`
	LogFormat          string `json:"log_format"`
	AccessKeysSet      bool   `json:"access_keys_set"`
	RateLimitRequests  int    `json:"rate_limit_requests"`
	RateLimitWindow    string `json:"rate_limit_window"`
	MaxStorageBytes    int64  `json:"max_storage_bytes"`
	ChunkSize          int64  `json:"chunk_size"`
	UploadSessionTTL   string `json:"upload_session_ttl"`
	VersioningEnabled  bool   `json:"versioning_enabled"`
	VersioningMax      int    `json:"versioning_max_versions"`
	CloudMaxConcurrent int    `json:"cloud_max_concurrent"`
	CloudSyncThreshold int64  `json:"cloud_sync_threshold"`
	HubEnabled         bool   `json:"hub_enabled"`
	TLSEnabled         bool   `json:"tls_enabled"`
	Addr               string `json:"addr"`
	StorageRoot        string `json:"storage_root"`
}

// GetConfig 获取远程服务器配置。
func (c *FileClient) GetConfig(ctx context.Context) (*ConfigResponse, error) {
	var cfg ConfigResponse
	if err := c.doJSON(ctx, "GET", "/api/config", nil, &cfg); err != nil {
		return nil, fmt.Errorf("获取配置失败: %w", err)
	}
	return &cfg, nil
}

// UpdateConfig 更新远程服务器运行时配置。
// 只更新指定的字段，未指定的字段保持不变。
// 可更新的字段：log_level, log_format, auth_token, rate_limit_requests, rate_limit_window。
func (c *FileClient) UpdateConfig(ctx context.Context, updates map[string]any) error {
	var result doJSONResp
	if err := c.doJSON(ctx, "PUT", "/api/config", updates, &result); err != nil {
		return fmt.Errorf("更新配置失败: %w", err)
	}
	return nil
}
