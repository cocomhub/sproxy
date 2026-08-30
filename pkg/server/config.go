// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"gopkg.in/yaml.v3"
)

// TLSConfig 是 TLS 相关配置，支持三种证书模式：
//   - CertFile + KeyFile：静态文件证书（最高优先级）
//   - ACME.Enabled：ACME 自动证书
//   - AutoTLS：自签证书（默认 fallback）
type TLSConfig struct {
	Enabled  bool       `yaml:"enabled" mapstructure:"enabled"`
	CertFile string     `yaml:"cert_file" mapstructure:"cert_file"`
	KeyFile  string     `yaml:"key_file" mapstructure:"key_file"`
	AutoTLS  bool       `yaml:"auto_tls" mapstructure:"auto_tls"`
	ClientCA string     `yaml:"client_ca" mapstructure:"client_ca"` // mTLS: CA 证书路径，非空时启用客户端证书验证
	ACME     ACMEConfig `yaml:"acme" mapstructure:"acme"`           // ACME 自动证书配置（可选）
}

// ACMEConfig 是 ACME 自动证书的配置。
type ACMEConfig struct {
	Enabled    bool     `yaml:"enabled" mapstructure:"enabled"`
	Domains    []string `yaml:"domains" mapstructure:"domains"`
	Email      string   `yaml:"email" mapstructure:"email"`
	CacheDir   string   `yaml:"cache_dir" mapstructure:"cache_dir"`
	HTTP01     bool     `yaml:"http01" mapstructure:"http01"`
	HTTP01Port string   `yaml:"http01_port" mapstructure:"http01_port"`
}

type RateLimitConfig struct {
	Enabled  bool          `yaml:"enabled" mapstructure:"enabled"`
	Requests int           `yaml:"requests" mapstructure:"requests"`
	Window   time.Duration `yaml:"window" mapstructure:"window"`
}

type ServerTimeouts struct {
	ReadHeader time.Duration `yaml:"read_header" mapstructure:"read_header"`
	Read       time.Duration `yaml:"read" mapstructure:"read"`
	Write      time.Duration `yaml:"write" mapstructure:"write"`
	Idle       time.Duration `yaml:"idle" mapstructure:"idle"`
	Shutdown   time.Duration `yaml:"shutdown" mapstructure:"shutdown"`
}

type VersionConfig struct {
	Enabled     bool `yaml:"enabled" mapstructure:"enabled"`
	MaxVersions int  `yaml:"max_versions" mapstructure:"max_versions"`
}

// DefaultHubTCPListen 是 hub 裸 TCP 中继的默认监听地址（transports.tcp.listen 为空时）。
// 与 sclient relay --transport tcp 无 --hub 的默认回落（127.0.0.1:18084）对齐。
//
// 安全边界：默认绑定 **loopback**（127.0.0.1）——裸 TCP 中继是网络面服务，全接口
// 绑定意味着任意网卡可达（属 SSRF/暴露面攻击目标）；远程节点可达需显式配置
// `listen: ":18084"` 或具体网卡 IP。注册准入由 SproxySig AccessKey + HMAC proof
// 保证（fail-closed：未配置 access_keys 时 hub 拒绝所有注册）。
const DefaultHubTCPListen = "127.0.0.1:18084"

// HubConfig 配置 Hub 中继系统。
// 节点注册准入由顶层 access_keys 提供（SproxySig AccessKey + HMAC proof），
// hub 级不再需要任何 token 配置。
type HubConfig struct {
	Enabled bool   `yaml:"enabled" mapstructure:"enabled"`
	NodeID  string `yaml:"node_id" mapstructure:"node_id"`
	// PersistFile 是 hub 状态持久化文件路径。非空时启用状态持久化
	// （节点注册/信令收件箱在重启间保留）；为空则持久化关闭（现有行为）。
	PersistFile string `yaml:"persist_file" mapstructure:"persist_file"`
	// MaxConnections 是 Hub 同时处理的中继节点连接数上限（I30），0 或不填使用默认 256。
	MaxConnections int              `yaml:"max_connections" mapstructure:"max_connections"`
	Transports     TransportConfigs `yaml:"transports" mapstructure:"transports"`
	// DHT 是节点发现表实现选择：""（默认）= 内置内存 DHT（不合并候选）；
	// "kad" = Kademlia（ext/kad，XOR 距离路由表）。启用 kad 后，注册节点喂入
	// DHT，/api/hub/nodes 合并 DHT 候选（路由表仍权威，DHT 只提供候选/发现）。
	DHT string `yaml:"dht" mapstructure:"dht"`
	// DHTSeeds 是 DHT 引导种子节点地址（仅 DHT=kad 时消费；空 = 不引导）。
	// 当前无 hub 联邦，种子用于未来多 hub DHT 组网；填入即调 Bootstrap 插入路由表。
	DHTSeeds []string `yaml:"dht_seeds" mapstructure:"dht_seeds"`
	// Federation 是 hub 联邦（hub-to-hub peering）配置。启用后本 hub 向配置的
	// 对端 hub 周期拉取节点表（联邦候选），并把本 hub 路由表节点表暴露给对端
	// （入站端点 /api/hub/federation/nodes，受 SproxySig 认证保护）。
	// 路由表仍本 hub 权威，联邦只提供发现/可达性，不改路由表状态。
	Federation FederationConfig `yaml:"federation" mapstructure:"federation"`
}

// FederationConfig 是 hub 联邦节点表同步的配置。
// Enabled 时注册入站联邦节点表端点（/api/hub/federation/nodes，按调用方 mesh
// 过滤返回本 hub 路由表节点）；Peers 非空时启动出站周期拉取。
type FederationConfig struct {
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
	// Peers 是联邦对端 hub 列表（出站拉取）。空 = 本 hub 仅作为被 peer（不主动拉取）。
	Peers []FederationPeerConfig `yaml:"peers" mapstructure:"peers"`
	// Interval 是出站拉取周期，默认 30s。
	Interval time.Duration `yaml:"interval" mapstructure:"interval"`
	// Timeout 是单次拉取超时，默认 10s。
	Timeout time.Duration `yaml:"timeout" mapstructure:"timeout"`
}

// FederationPeerConfig 是联邦对端 hub 的配置（出站拉取）。
// 认证复用 SproxySig AccessKey/AccessKeySecret（与 hub 节点注册准入同一模式）。
type FederationPeerConfig struct {
	// ID 是对端 hub 唯一标识（日志/去重用；为空回落 URL）。
	ID string `yaml:"id" mapstructure:"id"`
	// URL 是对端节点表端点基址（如 http://127.0.0.1:18083）。为空回落默认
	// loopback（http://127.0.0.1:18083）——远程 peering 必须显式配置 URL。
	URL string `yaml:"url" mapstructure:"url"`
	// AccessKey / AccessKeySecret 是对端 hub 认可的 SproxySig 凭据。
	// 目标 hub 配置了 access_keys 时必填；远程 peering（URL host 非 loopback）
	// 由 Validate 强制要求（fail-closed）。
	AccessKey       string `yaml:"access_key" mapstructure:"access_key"`
	AccessKeySecret string `yaml:"access_key_secret" mapstructure:"access_key_secret"`
	// CAFile 是对端 hub 的 TLS 受信 CA 证书文件路径（PEM）。非空时用该 CA 构建
	// 专属证书池严格校验对端证书（InsecureSkipVerify=false，ServerName 由 URL host
	// 自动校验）——自签 hub 的远程 peering 应配置 ca_file（受信 CA）而非跳过校验。
	// 为空时使用系统根证书池（默认，fail-closed：证书非法即拒绝）。与
	// InsecureSkipVerify 互斥。
	CAFile string `yaml:"ca_file" mapstructure:"ca_file"`
	// InsecureSkipVerify 为 true 时跳过 TLS 证书校验。**仅允许 loopback peer**
	// （本机自签开发/测试）；远程 peer 配置该选项由 Validate 拒绝（MITM 风险），
	// 远程自签场景应改用 ca_file。默认 false。
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" mapstructure:"insecure_skip_verify"`
}

// TransportConfigs 聚合所有可用的传输层配置。
type TransportConfigs struct {
	WS  WSTransportConfig  `yaml:"ws" mapstructure:"ws"`   // WebSocket 传输（挂载到主 HTTP server，固定 /ws）
	TCP TCPTransportConfig `yaml:"tcp" mapstructure:"tcp"` // 裸 TCP 中继传输（独立端口监听，默认关闭）
}

// WSTransportConfig 配置 WebSocket 传输监听。
// 当前 WS 升级端点挂载到主 HTTP server（与文件服务同端口，路径由 Path 指定），
// 因此 Listen 字段仅作预留（独立端口模式未来扩展），实际监听端口由 addr 决定。
type WSTransportConfig struct {
	Enabled bool   `yaml:"enabled" mapstructure:"enabled"`
	Listen  string `yaml:"listen" mapstructure:"listen"`
	// Path 已废弃（S36）：WS 升级路径固定为 /ws，配置非默认值不生效（启动时记录警告并忽略）。
	Path string `yaml:"path" mapstructure:"path"`
}

// TCPTransportConfig 配置裸 TCP 中继传输监听。
// 与 WS 不同，TCP 是独立 raw TCP listener（不走 HTTP server），端口由 Listen 指定。
// 默认关闭（Enabled=false），显式开启才生效。Listen 为空时回落默认
// 127.0.0.1:18084（loopback，与 sclient relay --transport tcp 无 --hub 的默认回落
// 一致）。远程节点可达需显式配置 `listen: ":18084"` 或具体网卡 IP（安全边界：默认
// 不绑定全部接口，见 DefaultHubTCPListen 注释）。
type TCPTransportConfig struct {
	Enabled bool   `yaml:"enabled" mapstructure:"enabled"`
	Listen  string `yaml:"listen" mapstructure:"listen"`
}

// WebConfig 控制 Web UI 的传输行为。
// Tunnel=true 时 Web 领域方法默认走加密隧道（由 SK 派生密钥）；false 走直连 SproxySig。
// 页面另有 localStorage 调试开关，可临时覆盖（仅调试用，非敏感开关可持久化）。
type WebConfig struct {
	Tunnel bool `yaml:"tunnel" mapstructure:"tunnel"`
}

// SyncConfig 是文件同步任务（SyncManager）配置。
type SyncConfig struct {
	// MaxConcurrent 最大并发同步任务数，默认 3。
	MaxConcurrent int `yaml:"max_concurrent" mapstructure:"max_concurrent"`
	// TaskTTL 完成任务保留时间，默认 24h。
	TaskTTL time.Duration `yaml:"task_ttl" mapstructure:"task_ttl"`
}

// SyncRemoteConfig 是同步远程节点配置（sync_remotes 数组元素）。
// URL 必须为 http(s)://host:port；AccessKey/AccessKeySecret 是远程 sproxy 认可的
// SproxySig 凭据（未配置时创建远程任务在 SyncManager 层 fail-closed 拒绝，Validate 只校验 URL/name）。
type SyncRemoteConfig struct {
	Name            string `yaml:"name" mapstructure:"name"`
	URL             string `yaml:"url" mapstructure:"url"`
	AccessKey       string `yaml:"access_key" mapstructure:"access_key"`
	AccessKeySecret string `yaml:"access_key_secret" mapstructure:"access_key_secret"`
}

type Config struct {
	Addr       string `yaml:"addr" mapstructure:"addr"`
	UploadsDir string `yaml:"uploads_dir" mapstructure:"uploads_dir"`
	// MaxUploadBytes 已移至 internal/size.UploadBodyLimit（1 GiB 硬限制），不可配置。
	// MaxChunkUploadBytes 已移至 internal/size.DefaultChunkBodyLimit（64 MiB 硬限制），不可配置。
	ServerTimeouts ServerTimeouts  `yaml:"server_timeouts" mapstructure:"server_timeouts"`
	LogLevel       string          `yaml:"log_level" mapstructure:"log_level"`
	LogFormat      string          `yaml:"log_format" mapstructure:"log_format"`
	MaxHeaderBytes int             `yaml:"max_header_bytes" mapstructure:"max_header_bytes"`
	TLS            TLSConfig       `yaml:"tls" mapstructure:"tls"`
	RateLimit      RateLimitConfig `yaml:"rate_limit" mapstructure:"rate_limit"`
	CORS           CORSConfig      `yaml:"cors" mapstructure:"cors"`

	// AccessKeys 是 SproxySig 请求签名认证的 AccessKey 配置（每 mesh 一对；
	// 替代旧 auth_token 明文 Bearer）。任一已配置即所有 HTTP 面（文件/信令/
	// 节点列表/服务发现）要求 SproxySig 签名。hub 侧为多 mesh 时配置多对。
	AccessKeys []AccessKeyConfig `yaml:"access_keys" mapstructure:"access_keys"`

	// 分块上传配置
	ChunkSize        int64         `yaml:"chunk_size" mapstructure:"chunk_size"`
	UploadSessionTTL time.Duration `yaml:"upload_session_ttl" mapstructure:"upload_session_ttl"`

	// 文件版本管理（默认关闭）
	Versioning VersionConfig `yaml:"versioning" mapstructure:"versioning"`

	// API 密钥配置
	APIKeys APIKeyConfig `yaml:"api_keys" mapstructure:"api_keys"`

	// Hub 中继系统（默认关闭）
	Hub HubConfig `yaml:"hub" mapstructure:"hub"`

	// Web UI 行为配置
	Web WebConfig `yaml:"web" mapstructure:"web"`

	// 文件同步任务配置（SyncManager）
	Sync        SyncConfig         `yaml:"sync" mapstructure:"sync"`
	SyncRemotes []SyncRemoteConfig `yaml:"sync_remotes" mapstructure:"sync_remotes"`

	// 存储空间控制
	MaxStorageBytes int64 `yaml:"max_storage_bytes" mapstructure:"max_storage_bytes"` // 存储上限（字节），0 = 不限制

	// 云端下载配置
	CloudSyncThreshold        int64         `yaml:"cloud_sync_threshold" mapstructure:"cloud_sync_threshold"`
	CloudDownloader           string        `yaml:"cloud_downloader" mapstructure:"cloud_downloader"`
	CloudTaskTTL              time.Duration `yaml:"cloud_task_ttl" mapstructure:"cloud_task_ttl"`
	CloudFailedTaskTTL        time.Duration `yaml:"cloud_failed_task_ttl" mapstructure:"cloud_failed_task_ttl"`
	CloudMaxConcurrent        int           `yaml:"cloud_max_concurrent" mapstructure:"cloud_max_concurrent"`
	CloudMaxBatchURLs         int           `yaml:"cloud_max_batch_urls" mapstructure:"cloud_max_batch_urls"`
	CloudDownloadAllowPrivate bool          `yaml:"cloud_download_allow_private" mapstructure:"cloud_download_allow_private"`
	CloudDownloadTimeout      time.Duration `yaml:"cloud_download_timeout" mapstructure:"cloud_download_timeout"`
	CloudDownloadIdleTimeout  time.Duration `yaml:"cloud_download_idle_timeout" mapstructure:"cloud_download_idle_timeout"`
	CloudMaxRetries           int           `yaml:"cloud_max_retries" mapstructure:"cloud_max_retries"`
	CloudRetryDelay           time.Duration `yaml:"cloud_retry_delay" mapstructure:"cloud_retry_delay"`
	// CloudArchiveMaxBytes 单次云归档允许的最大字节数（原始文件大小总和），0 = 不限制（仍受 max_storage_bytes 与 TryReserve 兜底）。
	CloudArchiveMaxBytes int64 `yaml:"cloud_archive_max_bytes" mapstructure:"cloud_archive_max_bytes"`
}

func Default() *Config {
	return &Config{
		Addr:       ":18083",
		UploadsDir: "./uploads",
		ServerTimeouts: ServerTimeouts{
			Shutdown: 30 * time.Second,
		},
		RateLimit: RateLimitConfig{
			Requests: 10,
			Window:   time.Second,
		},
		TLS: TLSConfig{
			Enabled: true,
			AutoTLS: true,
		},
		CORS: CORSConfig{
			MaxAge: defaultMaxAge,
		},
		Web: WebConfig{
			Tunnel: true,
		},
		Sync: SyncConfig{
			MaxConcurrent: 3,
			TaskTTL:       24 * time.Hour,
		},
		ChunkSize:                 size.DefaultChunkSize,
		UploadSessionTTL:          24 * time.Hour,
		CloudSyncThreshold:        20 * 1024 * 1024, // 20 MiB
		CloudDownloader:           "http",
		CloudTaskTTL:              24 * time.Hour,
		CloudFailedTaskTTL:        1 * time.Hour,
		CloudMaxConcurrent:        3,
		CloudMaxBatchURLs:         100,
		CloudDownloadAllowPrivate: false,
		CloudDownloadTimeout:      30 * time.Minute,
		CloudDownloadIdleTimeout:  1 * time.Minute,
		CloudMaxRetries:           10,
		CloudRetryDelay:           10 * time.Second,
	}
}

// SetDefaults 设置零值字段为默认值。
func (c *Config) SetDefaults() {
	if c.Addr == "" {
		c.Addr = ":18083"
	}
	if c.UploadsDir == "" {
		c.UploadsDir = "./uploads"
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = size.DefaultChunkSize
	}
	if c.UploadSessionTTL <= 0 {
		c.UploadSessionTTL = 24 * time.Hour
	}
	if c.ServerTimeouts.Shutdown <= 0 {
		c.ServerTimeouts.Shutdown = 30 * time.Second
	}
	if c.CloudSyncThreshold <= 0 {
		c.CloudSyncThreshold = 20 * 1024 * 1024
	}
	if c.CloudDownloader == "" {
		c.CloudDownloader = "http"
	}
	if c.CloudDownloadTimeout <= 0 {
		c.CloudDownloadTimeout = 30 * time.Minute
	}
	if c.CloudDownloadIdleTimeout <= 0 {
		c.CloudDownloadIdleTimeout = 1 * time.Minute
	}
	if c.CloudMaxRetries < 1 {
		c.CloudMaxRetries = 10
	}
	if c.CloudRetryDelay <= 0 {
		c.CloudRetryDelay = 10 * time.Second
	}
	if c.CloudMaxConcurrent <= 0 {
		c.CloudMaxConcurrent = 3
	}
	if c.CloudMaxBatchURLs == 0 {
		c.CloudMaxBatchURLs = 100
	}
	if c.CloudTaskTTL <= 0 {
		c.CloudTaskTTL = 24 * time.Hour
	}
	if c.CloudFailedTaskTTL <= 0 {
		c.CloudFailedTaskTTL = 1 * time.Hour
	}
	if c.Sync.MaxConcurrent <= 0 {
		c.Sync.MaxConcurrent = 3
	}
	if c.Sync.TaskTTL <= 0 {
		c.Sync.TaskTTL = 24 * time.Hour
	}
	if c.Hub.MaxConnections <= 0 {
		c.Hub.MaxConnections = 256
	}
	if c.Hub.Transports.TCP.Enabled && c.Hub.Transports.TCP.Listen == "" {
		c.Hub.Transports.TCP.Listen = DefaultHubTCPListen
	}
	if c.Hub.Federation.Interval <= 0 {
		c.Hub.Federation.Interval = 30 * time.Second
	}
	if c.Hub.Federation.Timeout <= 0 {
		c.Hub.Federation.Timeout = 10 * time.Second
	}
}

// Validate 校验配置合理性。
func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr 为空，请配置监听地址")
	}
	if c.UploadsDir == "" {
		return fmt.Errorf("uploads_dir 为空，请配置上传目录")
	}
	// 无 auth 配置（access_keys/api_keys 均为空）在 Validate 层是合法的——
	// fail-fast 拒绝启动在 cmd/sproxy 侧执行。
	if c.APIKeys.Enabled && len(c.APIKeys.Keys) == 0 {
		return fmt.Errorf("api_keys.enabled=true 但未配置任何密钥，认证将拒绝所有请求")
	}
	for i, k := range c.APIKeys.Keys {
		if k.Key == "" {
			return fmt.Errorf("api_keys[%d].key 为空，密钥不能为空字符串", i)
		}
		switch k.Permission {
		case PermissionRead, PermissionWrite, "":
		default:
			return fmt.Errorf("api_keys[%d].permission=%q 无效，仅允许 %q 或 %q", i, k.Permission, PermissionRead, PermissionWrite)
		}
	}
	// access_keys 校验（I-1/I-2）：Key 非空、Key 唯一、Secret 为 64 hex（32B）、
	// mesh_id 与 AK 内嵌 mesh 一致（防配置漂移导致两端隧道派生密钥不匹配）。
	seenAccessKeys := make(map[string]struct{}, len(c.AccessKeys))
	for i, k := range c.AccessKeys {
		if k.Key == "" {
			return fmt.Errorf("access_keys[%d].key 为空，密钥不能为空字符串", i)
		}
		if _, dup := seenAccessKeys[k.Key]; dup {
			return fmt.Errorf("access_keys[%d].key %q 重复", i, k.Key)
		}
		seenAccessKeys[k.Key] = struct{}{}
		if len(k.Secret) != 64 {
			return fmt.Errorf("access_keys[%d].secret 必须为 64 个十六进制字符（32 字节 AES 密钥源），got %d 字符", i, len(k.Secret))
		}
		if _, err := hex.DecodeString(k.Secret); err != nil {
			return fmt.Errorf("access_keys[%d].secret 不是合法十六进制: %v", i, err)
		}
		if k.MeshID != "" {
			if mesh := tunnel.AccessKeyMesh(k.Key); mesh != "" && mesh != k.MeshID {
				return fmt.Errorf("access_keys[%d].mesh_id %q 与 AK 内嵌 mesh %q 不一致（sclient 按 AK 解析 mesh 派生隧道密钥）", i, k.MeshID, mesh)
			}
		}
	}
	if c.RateLimit.Enabled && c.RateLimit.Requests <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 requests=%d 无效，请设置大于 0 的值", c.RateLimit.Requests)
	}
	if c.RateLimit.Enabled && c.RateLimit.Window <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 window=%s 无效，请设置大于 0 的 duration", c.RateLimit.Window)
	}
	if c.Hub.Enabled && !c.Hub.Transports.WS.Enabled && !c.Hub.Transports.TCP.Enabled {
		// S42 演进：节点接入传输 = ws（挂载主 HTTP server）或 tcp（独立 raw TCP
		// listener）。hub 启用而两者皆关时节点无法注册，属配置脚枪，fail-fast 启动失败。
		return fmt.Errorf("hub.enabled=true 但 transports.ws.enabled 与 transports.tcp.enabled 均为 false，中继节点无法连接，请至少启用一种传输")
	}
	if c.Hub.Enabled && c.Hub.Transports.TCP.Enabled && c.Hub.Transports.TCP.Listen != "" {
		// 端口冲突校验：TCP 中继是独立 raw TCP listener，不能与主 HTTP server（addr）
		// 同端口（同端口绑定会在启动时失败，这里提前给清晰错误）。比较 host:port 的
		// port 段；非 host:port 或 :0（随机端口）跳过（由 OS 绑定兜底）。
		if _, tcpPort, tcpErr := net.SplitHostPort(c.Hub.Transports.TCP.Listen); tcpErr == nil && tcpPort != "0" {
			if _, httpPort, httpErr := net.SplitHostPort(c.Addr); httpErr == nil && httpPort != "0" && tcpPort == httpPort {
				return fmt.Errorf("hub.transports.tcp.listen 端口 %s 与主 HTTP 监听 addr 端口 %s 冲突（TCP 中继与 HTTP server 不能同端口），请改配 transports.tcp.listen", tcpPort, httpPort)
			}
		}
	}
	if c.Hub.Enabled && c.Hub.DHT != "" && c.Hub.DHT != "kad" {
		// 防配置打错字（"kademlia" 等）被静默忽略。门控在 hub.enabled：hub 未启用时
		// dht 不被消费，历史/闲置配置遗留不阻断启动（与 ws transport 校验一致）。
		return fmt.Errorf("hub.dht=%q 无效，仅支持 \"\"（内置内存 DHT）或 \"kad\"（Kademlia）", c.Hub.DHT)
	}
	// hub 联邦配置校验（S4F）：URL 合法性 + 远程 peering 凭据强制（fail-closed）。
	// 门控在 hub.federation.enabled：hub 未启用或联邦关闭时 peers 不被消费，
	// 历史/闲置配置遗留不阻断启动。
	if c.Hub.Federation.Enabled {
		seenPeerIDs := make(map[string]struct{}, len(c.Hub.Federation.Peers))
		for i, p := range c.Hub.Federation.Peers {
			peerURL := p.URL
			if peerURL == "" {
				// 空 URL 回落默认 loopback（安全面：默认只与本机 hub peering）。
				// 仍以默认 URL 参与重复检测——两个空 URL peer 都回落同一默认
				// 地址属配置冲突（运行时后写覆盖），启动时拦截。
				peerURL = hub.DefaultFederationPeerURL
			}
			u, perr := url.Parse(peerURL)
			if perr != nil {
				return fmt.Errorf("hub.federation.peers[%d].url 非法: %v", i, perr)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("hub.federation.peers[%d].url scheme %q 无效，仅允许 http/https", i, u.Scheme)
			}
			if p.URL != "" && !isLoopbackHost(u.Hostname()) && (p.AccessKey == "" || p.AccessKeySecret == "") {
				// 远程 peering 必须显式成对配置凭据（AccessKey + AccessKeySecret）——
				// 缺失任一即无有效签名，未配置时无认证直连远程 hub 属暴露面，fail-closed 拒绝。
				return fmt.Errorf("hub.federation.peers[%d].url %q 为远程地址，远程 peering 必须同时配置 access_key 与 access_key_secret", i, p.URL)
			}
			if p.AccessKeySecret != "" {
				// 与顶层 access_keys 的校验一致：SK 必须为 64 hex（32 字节 HMAC 密钥源）。
				if len(p.AccessKeySecret) != 64 {
					return fmt.Errorf("hub.federation.peers[%d].access_key_secret 必须为 64 个十六进制字符（32 字节），got %d 字符", i, len(p.AccessKeySecret))
				}
				if _, derr := hex.DecodeString(p.AccessKeySecret); derr != nil {
					return fmt.Errorf("hub.federation.peers[%d].access_key_secret 不是合法十六进制: %v", i, derr)
				}
			}
			// TLS 安全边界（S-Medium 闭环）：insecure_skip_verify 仅限 loopback peer
			// （本机自签开发/测试）；远程 peer 必须严格校验 TLS（受信任证书或 ca_file），
			// 跳过校验 = MITM 可窃听/篡改节点表，fail-closed 拒绝。
			if p.InsecureSkipVerify && !isLoopbackHost(u.Hostname()) {
				return fmt.Errorf("hub.federation.peers[%d].insecure_skip_verify 仅允许用于 loopback peer（本机自签开发）；远程 peering 应配置受信任证书或 ca_file（受信 CA）", i)
			}
			// ca_file 与 insecure_skip_verify 互斥（ca_file 是严格校验，跳过校验与其冲突）。
			if p.CAFile != "" && p.InsecureSkipVerify {
				return fmt.Errorf("hub.federation.peers[%d].ca_file 与 insecure_skip_verify 互斥，请二选一（ca_file 为受信 CA 严格校验）", i)
			}
			if p.CAFile != "" {
				if _, serr := os.Stat(p.CAFile); serr != nil {
					return fmt.Errorf("hub.federation.peers[%d].ca_file %q 不可读: %v", i, p.CAFile, serr)
				}
			}
			key := p.ID
			if key == "" {
				key = peerURL
			}
			if _, dup := seenPeerIDs[key]; dup {
				return fmt.Errorf("hub.federation.peers[%d].id %q 重复", i, p.ID)
			}
			seenPeerIDs[key] = struct{}{}
		}
	}
	// sync_remotes 校验：URL 合法（http/https + host）、name 唯一非空。
	// 凭据 fail-closed 在 SyncManager.CreateTask 层执行（Validate 不要求凭据——
	// 允许配置空凭据的 remote 供未启用 access_keys 的远程节点使用，创建任务时才拒绝）。
	seenSyncRemoteNames := make(map[string]struct{}, len(c.SyncRemotes))
	for i, r := range c.SyncRemotes {
		if r.Name == "" {
			return fmt.Errorf("sync_remotes[%d].name 为空，名称不能为空字符串", i)
		}
		if _, dup := seenSyncRemoteNames[r.Name]; dup {
			return fmt.Errorf("sync_remotes[%d].name %q 重复", i, r.Name)
		}
		seenSyncRemoteNames[r.Name] = struct{}{}
		u, perr := url.Parse(r.URL)
		if perr != nil {
			return fmt.Errorf("sync_remotes[%d].url 非法: %v", i, perr)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("sync_remotes[%d].url scheme %q 无效，仅允许 http/https", i, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("sync_remotes[%d].url 缺少 host: %q", i, r.URL)
		}
	}
	return nil
}

// isLoopbackHost 判断主机名是否为 loopback（IPv4/IPv6 loopback 或 localhost）。
// 用于联邦 peering 的安全边界：默认 loopback 安全面，远程 peering 需显式配置。
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	// net.SplitHostPort 对 IPv6 返回带方括号的 host（如 "[::1]"），strip 后判断。
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadFromProvider 从 provider.Provider 解码配置，设置默认值并校验。
func LoadFromProvider(p provider.Provider) (*Config, error) {
	cfg := Default()
	if err := p.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解码失败: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfig 加载配置文件。路径为空或文件不存在时返回默认配置，不自动创建文件。
func LoadConfig(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
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
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}

	return cfg, nil
}

func SaveConfig(cfg *Config, path string) error {
	// TODO: 后续优化敏感信息管理（AuthToken 脱敏）
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	return nil
}
