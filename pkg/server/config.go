// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/storage"
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

// AuditConfig 是有界内存环形审计缓冲配置（audit.buffer_size）。
// BufferSize 为保留的审计事件条数上限（环形覆盖）。默认 2048（SetDefaults 填充，
// 意味着默认启用 ring）；0 = 显式关闭 ring（GET /api/audit 返回空表）；负值非法。
// 仅内存缓冲，不落盘（审计留档交给日志 collector 消费 RecordAudit 的 JSON 行）。
type AuditConfig struct {
	BufferSize int `yaml:"buffer_size" mapstructure:"buffer_size"`
}

// OTELConfig 是 OpenTelemetry SDK 装配配置（观测面，非文件功能）。
// 默认关闭（telemetry.enabled=false）时 server 保持纯 slog 日志，零额外开销；
// 开启时由 autoexport 按标准环境变量（OTEL_TRACES_EXPORTER /
// OTEL_EXPORTER_OTLP_ENDPOINT 等）创建真实 exporter。OTLPEndpoint 提供
// 配置层面的显式覆写（优先于环境变量，见 provider.NewProvider）。
// SampleRatio 是 ParentBased(TraceIDRatioBased) 采样率，∈ (0,1]。
type OTELConfig struct {
	Enabled      bool    `yaml:"enabled" mapstructure:"enabled"`
	SampleRatio  float64 `yaml:"sample_ratio" mapstructure:"sample_ratio"`
	OTLPEndpoint string  `yaml:"otlp_endpoint" mapstructure:"otlp_endpoint"`
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
// 保证（fail-closed：凭据 Ring 空时 hub 拒绝所有注册）。
const DefaultHubTCPListen = "127.0.0.1:18084"

// DefaultXferTCPListen 是 xfer_tcp 明文 listener 的默认监听地址
// （transports.xfer_tcp.listen 为空时）。
//
// 安全边界：默认绑定 **loopback**（127.0.0.1）——xfer listener 承载文件内容
// （文件 API 隧道面），全接口绑定意味着任意网卡可达（数据泄露面）；远程节点可达需
// 显式配置 `listen: ":<port>"`。明文 tcp 仅显式 option，TLS 机密性 + Ed25519 指纹
// pinning 由 xfer_tls / tls_enabled 保证。
const DefaultXferTCPListen = "127.0.0.1:18086"

// DefaultXferTLSListen 是 xfer_tls listener 的默认监听地址
// （transports.xfer_tls.listen 为空时）。同样默认绑 loopback。
const DefaultXferTLSListen = "127.0.0.1:18087"

// HubConfig 配置 Hub 中继系统。
// 节点注册准入由凭据 Ring 提供（SproxySig AccessKey + HMAC proof），
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
	// VirtualSubnet 是虚拟 IP 分配的 IPv4 子网（CIDR）。默认 CGNAT 100.64.0.0/10
	// （RFC 6598，Tailscale 同款）；空值由 SetDefaults 填默认。Validate 限定 IPv4。
	// 子网首地址（.1）保留为网关/默认，实际分配从 .2 起。
	VirtualSubnet string `yaml:"virtual_subnet" mapstructure:"virtual_subnet"`
	// DHT 是节点发现表实现选择：""（默认）= 内置内存 DHT（不合并候选）；
	// "kad" = Kademlia（ext/kad，XOR 距离路由表）。启用 kad 后，注册节点喂入
	// DHT，/api/hub/nodes 合并 DHT 候选（路由表仍权威，DHT 只提供候选/发现）。
	DHT string `yaml:"dht" mapstructure:"dht"`
	// DHTSeeds 是 DHT 引导种子节点地址（仅 DHT=kad 时消费；空 = 不引导）。
	// 当前无 hub 联邦，种子用于未来多 hub DHT 组网；填入即调 Bootstrap 插入路由表。
	DHTSeeds []string `yaml:"dht_seeds" mapstructure:"dht_seeds"`
	// DHTPersistFile 是 kad DHT 路由表（k-bucket）持久化文件路径。非空且
	// hub.dht=kad 时启用 k-bucket 落盘（重启后恢复上次发现缓存，不冷启动）；
	// 为空则持久化关闭（现有行为）。快照只存 id/route_id/addr（发现缓存无
	// secret），损坏/缺失/超限文件按空桶启动。路由表仍 hub 权威，DHT 持久化
	// 是缓存语义。
	DHTPersistFile string `yaml:"dht_persist_file" mapstructure:"dht_persist_file"`
	// Federation 是 hub 联邦（hub-to-hub peering）配置。启用后本 hub 向配置的
	// 对端 hub 周期拉取节点表（联邦候选），并把本 hub 路由表节点表暴露给对端
	// （入站端点 /api/hub/federation/nodes，受 SproxySig 认证保护）。
	// 路由表仍本 hub 权威，联邦只提供发现/可达性，不改路由表状态。
	Federation FederationConfig `yaml:"federation" mapstructure:"federation"`
	// XferIdentityFile 是服务端 xfer listener 的 Ed25519 身份文件路径（AD-4：
	// 用于握手指纹 pinning）。为空时回落到 XDG 用户配置目录
	// （os.UserConfigDir()/sproxy/server-identity.json），**绝不放 storage_root 下**
	// （与文件 API 命名空间重叠，已认证 peer 可经 /download 读取私钥——审查 C-1）。
	XferIdentityFile string `yaml:"xfer_identity_file" mapstructure:"xfer_identity_file"`
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
	// PersistFile 是联邦候选节点表持久化文件路径。非空时启用候选持久化（重启后恢复
	// 上次同步的候选节点，不冷启动）；为空则持久化关闭（现有行为）。快照只存
	// id/addr/mesh（发现缓存无 secret），损坏/缺失文件按空候选启动。
	PersistFile string `yaml:"persist_file" mapstructure:"persist_file"`
}

// FederationPeerConfig 是联邦对端 hub 的配置（出站拉取）。
// 认证复用 SproxySig AccessKey/AccessKeySecret（与 hub 节点注册准入同一模式）。
type FederationPeerConfig struct {
	// ID 是对端 hub 唯一标识（日志/去重用；为空回落 URL）。
	ID string `yaml:"id" mapstructure:"id"`
	// URL 是对端节点表端点基址（如 http://127.0.0.1:18083）。为空回落默认
	// loopback（http://127.0.0.1:18083）——远程 peering 必须显式配置 URL。
	URL string `yaml:"url" mapstructure:"url"`
	// AccessKey / AccessKeySecret / AccessKeyID 是对端 hub 认可的 SproxySig 凭据
	// （AccessKeyID 是 SK 条目 skeyID，v2 协议必传）。目标 hub 凭据 Ring 非空时
	// 必填；远程 peering（URL host 非 loopback）由 Validate 强制要求（fail-closed）。
	AccessKey       string `yaml:"access_key" mapstructure:"access_key"`
	AccessKeySecret string `yaml:"access_key_secret" mapstructure:"access_key_secret"`
	AccessKeyID     string `yaml:"access_key_id" mapstructure:"access_key_id"`
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
	// XferTCP/XferTLS 是服务端 xfer listener（阶段 5 工作项 1）：接收
	// `sclient tunnel --xfer tcp/tcp+tls --hub <addr>` 的会话，经 mux → tunnel 解密 →
	// 路由到本地文件 API。与 hub 中继不同，xfer listener 不参与节点注册/VIP/DHT。
	XferTCP XferTransportConfig `yaml:"xfer_tcp" mapstructure:"xfer_tcp"` // 明文 xfer listener（显式 option；tls_enabled 可升级）
	XferTLS XferTransportConfig `yaml:"xfer_tls" mapstructure:"xfer_tls"` // TLS xfer listener（默认，段名即约定恒 TLS）
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

// XferTransportConfig 配置服务端 xfer listener（阶段 5 工作项 1）。
//
// 两段（xfer_tcp / xfer_tls）共用本结构：
//   - xfer_tls 段恒为 TLS（段名即约定），TLSEnabled 不消费；
//   - xfer_tcp 段默认为明文（裸 tcp，显式 option），TLSEnabled=true 时升级为 TLS
//     （复用 tcp+tls 传输，与 xfer_tls 同一证书源 cfg.TLS.*）。
//
// Listen 为空时回落 loopback 默认地址（DefaultXferTCPListen / DefaultXferTLSListen）——
// xfer listener 承载文件内容，默认不绑全部接口，远程可达须显式 listen。
type XferTransportConfig struct {
	Enabled bool   `yaml:"enabled" mapstructure:"enabled"`
	Listen  string `yaml:"listen" mapstructure:"listen"`
	// TLSEnabled 仅 xfer_tcp 段有意义：true 把明文 xfer listener 升级为 TLS。
	TLSEnabled bool `yaml:"tls_enabled" mapstructure:"tls_enabled"`
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
	// MaxRetries 瞬时网络错误（可重试）最大重试次数，默认 10（对齐 cloud retry）。
	MaxRetries int `yaml:"max_retries" mapstructure:"max_retries"`
	// RetryDelay 指数退避基准延迟（第 1 次重试的等待时长），默认 10s。
	RetryDelay time.Duration `yaml:"retry_delay" mapstructure:"retry_delay"`
	// RetryBackoff 指数退避倍率（每次重试延迟乘以此值，如 2 = 2x），默认 2。
	// 退避上限封顶 RetryDelay*10（对齐 cloud 重试预算，避免长尾任务卡死）。
	RetryBackoff float64 `yaml:"retry_backoff" mapstructure:"retry_backoff"`
}

// SyncRemoteConfig 是同步远程节点配置（sync_remotes 数组元素）。
// URL 必须为 http(s)://host:port；AccessKey/AccessKeySecret 是远程 sproxy 认可的
// SproxySig 凭据（未配置时创建远程任务在 SyncManager 层 fail-closed 拒绝，Validate 只校验 URL/name）。
type SyncRemoteConfig struct {
	Name            string `yaml:"name" mapstructure:"name"`
	URL             string `yaml:"url" mapstructure:"url"`
	AccessKey       string `yaml:"access_key" mapstructure:"access_key"`
	AccessKeySecret string `yaml:"access_key_secret" mapstructure:"access_key_secret"`
	AccessKeyID     string `yaml:"access_key_id" mapstructure:"access_key_id"`
}

// RegistrationConfig 是注册（凭据登记）相关配置。
// Disable 缺省 false = 允许注册（默认，首启 anonymous 凭据生成）；true = 禁止注册
// （仅存量用户，无法新增用户）。字段命名避免"allow=false 表示允许"的反直觉语义。
// ForceTOTP（yaml force_totp，默认 false）：true 时 register 走 TOTP 分支——注册
// 只下发 AK + otpauth_uri/base32_secret（用户须经 TOTP 登录拿 session SK，DEC-B），
// 不直接下发明文 SK。false（默认）= 简单 AK/SK 模式（register 直接下发 SK）。
type RegistrationConfig struct {
	Disable bool `yaml:"disable" mapstructure:"disable"`
	// ForceTOTP 强制 TOTP 注册（DEC-B）：默认 false = 简单 AK/SK 注册（4A 语义
	// 回归）；true = 仅按 TOTP 注册（不建 SK 条目，见 register_handler.go）。
	ForceTOTP bool `yaml:"force_totp" mapstructure:"force_totp"`
	// SessionTTL 是 TOTP 登录（login_type=web，缺省）签发的 session SK 有效期
	// （D3 服务端控 TTL；默认 24h）。
	SessionTTL time.Duration `yaml:"session_ttl" mapstructure:"session_ttl"`
	// CliTTL 是 login_type=cli（sclient trust login 回填）的 session SK 有效期
	// （默认 7d——CLI 长期运行 daemon 需要更长会话磨损窗口）。
	CliTTL time.Duration `yaml:"cli_ttl" mapstructure:"cli_ttl"`
	// LoginFailLimit 是 per-AK 失败锁定阈值（U4）：该 AK 连续失败达此次数即进入
	// 锁定窗口（默认 5）。锁定窗口内该 AK 的登录一律 401（含正确动态码，不随 IP
	// 变化失效——防分布式 botnet 爆破）。
	LoginFailLimit int `yaml:"login_fail_limit" mapstructure:"login_fail_limit"`
	// LoginFailWindow 是 per-AK 失败锁定期（默认 15m，U4）：达 LoginFailLimit 后
	// 锁定 LoginFailWindow 时长，到期自动解锁（无需管理员介入）。
	LoginFailWindow time.Duration `yaml:"login_fail_window" mapstructure:"login_fail_window"`
}

// VaultConfig 是 Vault Transit 后端子配置（credential_store.vault 段，
// backend=vault + encrypt=true 时必需）。
type VaultConfig struct {
	Addr    string `yaml:"addr" mapstructure:"addr"`
	Mount   string `yaml:"mount" mapstructure:"mount"` // transit engine 挂载（默认 "transit"）
	KeyName string `yaml:"key_name" mapstructure:"key_name"`
	// TokenFile 是 Vault token 文件路径（读取后 trim）；为空时回落 TokenEnv 环境变量。
	TokenFile string        `yaml:"token_file" mapstructure:"token_file"`
	TokenEnv  string        `yaml:"token_env" mapstructure:"token_env"` // 默认 "VAULT_TOKEN"
	CAFile    string        `yaml:"ca_file" mapstructure:"ca_file"`     // 自签 CA PEM 路径（可选，默认系统池）
	Timeout   time.Duration `yaml:"timeout" mapstructure:"timeout"`     // 默认 10s
	// CacheTTL 是 VaultTransitStorer decrypt 结果缓存 TTL（默认 30s）。viper 零值歧义
	// 无法区分「未设」与「显式 0」——SetDefaults 对 <=0 一律回落 30s（负值同视为未设），
	// 故 config 层缓存恒默认开、不可显式关（文档注明）；VaultOptions.CacheTTL 内部 API
	// 可传 0 关闭（测试用）。
	CacheTTL time.Duration `yaml:"cache_ttl" mapstructure:"cache_ttl"`
}

// CredentialStoreConfig 是凭据静态存储加密配置（credential_store 段，4C-2）。
// Encrypt 缺省 false = 明文（现状 server.CredentialStore，零回归）；true = 装配
// accesskey.EncryptingStorer 对 <tenant>/meta/credentials.json 做字节级加密静态存储，
// backend 选择加密后端：
//
//   - backend=aesgcm（缺省/空）：本地 AES-256-GCM 加密（master_key_file 或环境变量
//     CredentialMasterKeyEnv 作 master key，见下）；
//   - backend=vault：HashiCorp Vault Transit 引擎加解密（密钥永不出 Vault，见 VaultConfig），
//     忽略 master_key_file，要求 vault 子段 addr/key_name/token 源齐全。
//
// master key 说明（aesgcm）：Encrypt=true + backend=aesgcm 必须能解析出 32B master key
// （base64 32B 或 raw 32B）——优先 master_key_file（文件可读性在装配层校验）；为空时回落
// 环境变量 CredentialMasterKeyEnv（base64 32B）。本任务不做口令派生路径（无独立盐配置），
// 该 32B 直接作 AES-256 key（EncryptWithKey 内部随机 nonce 已提供语义安全）。
type CredentialStoreConfig struct {
	Encrypt bool `yaml:"encrypt" mapstructure:"encrypt"`
	// Backend 是加密后端枚举：aesgcm（缺省）| vault；空 → SetDefaults 归一 aesgcm。
	Backend string `yaml:"backend" mapstructure:"backend"`
	// MasterKeyFile 是 master key 文件路径（base64 32B 或 raw 32B，见
	// accesskey.LoadMasterKeyFromFile）。backend=aesgcm 时为空回落 CredentialMasterKeyEnv。
	MasterKeyFile string `yaml:"master_key_file" mapstructure:"master_key_file"`
	// Vault 是 backend=vault 时的子配置（见 VaultConfig）。
	Vault VaultConfig `yaml:"vault" mapstructure:"vault"`
}

// CredentialMasterKeyEnv 是 credential_store 加密装配的 master key 环境变量名
// （base64 编码 32B）。master_key_file 非空时优先读文件；仅文件未配置时读本变量。
const CredentialMasterKeyEnv = "SPROXY_CREDENTIAL_MASTER_KEY"

// defaultStorageRoot 是 storage_root 与合成默认卷的缺省挂载根。Default()/SetDefaults()
// 共用同一常量（单一事实源），避免魔数在多处漂移。
const defaultStorageRoot = "./storage"

// VolumeACLMode 是卷 ACL 模式。allow=默认拒绝+白名单；deny=默认开放+黑名单。
type VolumeACLMode string

const (
	VolumeACLAllow VolumeACLMode = "allow"
	VolumeACLDeny  VolumeACLMode = "deny"
)

// VolumeACLConfig 是卷 ACL 配置（volumes[].acl）。
// Mode 缺省 deny（默认开放，单卷零回归）；显式 allow 时仅列出的 owner 可写入该卷。
// Owners 是 owner 白/黑名单（按 owner 名段名校验，语义由装配层按 Mode 解释）。
//
// 安全边界（security MEDIUM 文档化落点，行为有意维持）：未配置/空 ACL（Mode=deny +
// 空 owners）= **默认开放**——这是为兼容默认卷/旧单根布局的有意语义（规格 AD-6，
// 2026-09-08 裁定维持全开放）；生产环境对敏感卷请显式 `mode: allow` 白名单收紧。
type VolumeACLConfig struct {
	Mode   VolumeACLMode `yaml:"mode" mapstructure:"mode"`
	Owners []string      `yaml:"owners" mapstructure:"owners"`
}

// VolumeConfig 是单卷配置（volumes[] 元素）：独立挂载根 + 卷容量上限 + ACL。
// Name 为卷唯一标识（复用 storage.ValidSegmentName 段名规则，见 Validate）；
// Root 为该卷独立存储根（含 <tenant>/ 六桶布局）；VolCapacity 为该卷字节上限
// （0 = 不限制，仍受租户 owner_quotas 与 max_storage_bytes 兜底）。
type VolumeConfig struct {
	Name        string           `yaml:"name" mapstructure:"name"`
	Root        string           `yaml:"root" mapstructure:"root"`
	VolCapacity int64            `yaml:"vol_capacity" mapstructure:"vol_capacity"`
	ACL         *VolumeACLConfig `yaml:"acl,omitempty" mapstructure:"acl"`
}

type Config struct {
	Addr string `yaml:"addr" mapstructure:"addr"`
	// StorageRoot 是存储根目录（新布局 <root>/<tenant>/{user,cloud,...}/）。
	// YAML 键为 storage_root（字段与 YAML 键一致，直接字段访问）。
	StorageRoot string `yaml:"storage_root" mapstructure:"storage_root"`
	// OwnerQuotas 是 per-tenant 配额上限（字节），key 为 owner 名（含 "anonymous"），
	// "*" 为默认值（未显式列出的 owner 用此值）；0 = 不限制。
	// 启动装配时按此创建各租户的配额 Scope（quotaFor 懒创建）。
	OwnerQuotas map[string]int64 `yaml:"owner_quotas" mapstructure:"owner_quotas"`
	// BucketLimits 是功能桶/子目录配额上限（字节），key 为相对租户根路径
	// （如 "user/videos/hd" → <tenant>/user/videos/hd 下字节上限；也支持功能桶根
	// "cloud"）。0 = 该路径不单独限制（仍受租户总 owner_quotas 与全局
	// max_storage_bytes 兜底）。启动装配时按此创建路径子 Scope（quotaBucketFor 懒建）；
	// 仅装配期消费，SIGHUP 后不重建 → bucket_limits 修改需重启进程。
	BucketLimits map[string]int64 `yaml:"bucket_limits" mapstructure:"bucket_limits"`
	// Placement 卷路由策略 prefer-default|spread（缺省 prefer-default）。
	Placement string `yaml:"placement" mapstructure:"placement"`
	// Volumes 卷列表；缺省（nil/空）由 Normalize/Default 合成单默认卷
	// （name=default, root=StorageRoot），YAML 未配 volumes 时行为与单根布局一致。
	Volumes []VolumeConfig `yaml:"volumes" mapstructure:"volumes"`
	// MaxUploadBytes 已移至 internal/size.UploadBodyLimit（1 GiB 硬限制），不可配置。
	// MaxChunkUploadBytes 已移至 internal/size.DefaultChunkBodyLimit（64 MiB 硬限制），不可配置。
	ServerTimeouts ServerTimeouts  `yaml:"server_timeouts" mapstructure:"server_timeouts"`
	LogLevel       string          `yaml:"log_level" mapstructure:"log_level"`
	LogFormat      string          `yaml:"log_format" mapstructure:"log_format"`
	MaxHeaderBytes int             `yaml:"max_header_bytes" mapstructure:"max_header_bytes"`
	TLS            TLSConfig       `yaml:"tls" mapstructure:"tls"`
	RateLimit      RateLimitConfig `yaml:"rate_limit" mapstructure:"rate_limit"`
	// Telemetry 是 OpenTelemetry 观测装配配置（telemetry.enabled）。
	// telemetry 是比 tracing 更广的 umbrella：当前仅 OTELConfig（trace），
	// 命名空间为未来扩展 metric/log 观测类型预留。默认关闭。
	Telemetry OTELConfig `yaml:"telemetry" mapstructure:"telemetry"`
	CORS      CORSConfig `yaml:"cors" mapstructure:"cors"`

	// 注册/无认证兜底配置（凭据 store 化后取代 yaml access_keys）：
	//   - Registration.Disable 为 false（缺省）= 允许注册；true = 禁止注册（仅存量用户）。
	//     anonymous 首启生成由 credentialRing.Len()==0 触发（见 RegisterRoutes），
	//     Disable 不阻止 anonymous 生成——它是新部署可访问凭据的保证。
	//   - AllowInsecureLoopback 仅用于无任何凭据（ring 空）时的本地调试：放行
	//     loopback 来源的 GET/HEAD，其余 401。生产勿开。
	//   - CredentialTTL 是新建 SK 条目有效期（renew 新 SK 用，服务端控 TTL；默认 30d）。
	Registration          RegistrationConfig `yaml:"registration" mapstructure:"registration"`
	AllowInsecureLoopback bool               `yaml:"allow_insecure_loopback" mapstructure:"allow_insecure_loopback"`
	CredentialTTL         time.Duration      `yaml:"credential_ttl" mapstructure:"credential_ttl"`

	// CredentialStore 是凭据静态存储加密配置（credential_store 段，4C-2）。
	// Encrypt=true 时凭据文件以加密字节落盘（BootstrapServerCredentials 装配
	// EncryptingStorer）；backend 选择 aesgcm（本地 AES-256-GCM，缺省）或 vault（Vault
	// Transit），默认明文零回归。
	CredentialStore CredentialStoreConfig `yaml:"credential_store" mapstructure:"credential_store"`

	// 分块上传配置
	ChunkSize        int64         `yaml:"chunk_size" mapstructure:"chunk_size"`
	UploadSessionTTL time.Duration `yaml:"upload_session_ttl" mapstructure:"upload_session_ttl"`

	// 文件版本管理（默认关闭）
	Versioning VersionConfig `yaml:"versioning" mapstructure:"versioning"`

	// Audit 是有界内存环形审计缓冲配置（audit.buffer_size，默认 2048）。
	Audit AuditConfig `yaml:"audit" mapstructure:"audit"`

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

// OwnerQuotaFor 返回指定 owner 的配额上限（字节）：显式 owner 配置 > "*" 默认值 > 0。
// 未配置任何 owner_quotas 时返回 0（不限制）。
func (c *Config) OwnerQuotaFor(owner string) int64 {
	if c.OwnerQuotas == nil {
		return 0
	}
	if v, ok := c.OwnerQuotas[owner]; ok {
		return v
	}
	return c.OwnerQuotas["*"]
}

func Default() *Config {
	return &Config{
		Addr:        ":18083",
		StorageRoot: defaultStorageRoot,
		// OwnerQuotas/BucketLimits 默认空 map（非 nil，便于 map 判断/访问复用）。
		OwnerQuotas:  map[string]int64{},
		BucketLimits: map[string]int64{},
		// Placement/Volumes：缺省 prefer-default + 合成单默认卷（root=StorageRoot）。
		// Default() 即产出归一后的单卷形态（SetDefaults 对 len==0/占位单卷再次兜底），
		// 保证直接消费 Default() 的路径总能看到非空 Volumes。
		Placement: "prefer-default",
		Volumes:   []VolumeConfig{{Name: "default", Root: defaultStorageRoot}},
		ServerTimeouts: ServerTimeouts{
			Shutdown: 30 * time.Second,
		},
		RateLimit: RateLimitConfig{
			Requests: 10,
			Window:   time.Second,
		},
		Telemetry: OTELConfig{
			Enabled:      false,
			SampleRatio:  1.0,
			OTLPEndpoint: "",
		},
		TLS: TLSConfig{
			Enabled: true,
			AutoTLS: true,
		},
		CORS: CORSConfig{
			MaxAge: defaultMaxAge,
		},
		Registration: RegistrationConfig{
			Disable:         false,
			ForceTOTP:       false,
			SessionTTL:      24 * time.Hour,     // TOTP 登录 web session 默认 24h（D3）
			CliTTL:          7 * 24 * time.Hour, // CLI 回填 session 默认 7d（D3）
			LoginFailLimit:  5,                  // per-AK 失败锁定阈值（U4）
			LoginFailWindow: 15 * time.Minute,   // 锁定窗口 15m（U4）
		},
		CredentialTTL:         30 * 24 * time.Hour, // 新建 SK 条目有效期（renew 新 SK 用，服务端控 TTL；默认 30d）
		AllowInsecureLoopback: false,
		CredentialStore: CredentialStoreConfig{
			Backend: "aesgcm", // 缺省本地 AES-256-GCM；vault = Vault Transit
			Vault: VaultConfig{
				Mount:    "transit",     // transit engine 缺省挂载路径
				TokenEnv: "VAULT_TOKEN", // token 环境变量名
				Timeout:  10 * time.Second,
				CacheTTL: 30 * time.Second, // decrypt 缓存默认 30s（viper 零值歧义：<=0 回落 30s，config 不可关缓存）
			},
		},
		Web: WebConfig{
			Tunnel: true,
		},
		Sync: SyncConfig{
			MaxConcurrent: 3,
			TaskTTL:       24 * time.Hour,
			MaxRetries:    10,
			RetryDelay:    10 * time.Second,
			RetryBackoff:  2,
		},
		Audit: AuditConfig{
			BufferSize: 2048,
		},
		Hub: HubConfig{
			VirtualSubnet: hub.DefaultVirtualSubnet,
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
	if c.StorageRoot == "" {
		c.StorageRoot = defaultStorageRoot
	}
	// 卷配置归一（多卷）：placement 缺省 prefer-default；volumes 未配（nil/空）合成
	// 单默认卷（name=default, root=StorageRoot）。已配卷时逐卷补**空** root（仅首卷，
	// 跟随 storage_root；非首卷空 root 保留 → Validate 拒绝）与缺省 ACL（mode 缺省
	// deny = 默认开放，单卷零回归）。绝不覆写非空显式 root——用户显式写的
	// volumes[].root（含首卷）恒保留。
	if c.Placement == "" {
		c.Placement = "prefer-default"
	}
	if len(c.Volumes) == 0 {
		c.Volumes = []VolumeConfig{{Name: "default", Root: c.StorageRoot}}
	}
	for i := range c.Volumes {
		if c.Volumes[i].Root == "" && i == 0 {
			c.Volumes[i].Root = c.StorageRoot
		}
		// 非首卷空 root 保留 → Validate 拒绝（非首卷需显式指定挂载根）
		if c.Volumes[i].ACL == nil {
			c.Volumes[i].ACL = &VolumeACLConfig{Mode: VolumeACLDeny} // 缺省开放
		}
		if c.Volumes[i].ACL.Mode == "" {
			c.Volumes[i].ACL.Mode = VolumeACLDeny
		}
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
	if c.CredentialTTL == 0 {
		c.CredentialTTL = 30 * 24 * time.Hour
	}
	// credential_store 子配置默认（4C-2 / Vault Transit）：backend 空 → aesgcm；vault 子段
	// mount/token_env/timeout/cache_ttl 零值回落。CacheTTL 用 <=0 → 30s（viper 零值歧义，
	// config 层缓存恒默认开、不可显式关，见 VaultConfig.CacheTTL 注释）。
	if c.CredentialStore.Backend == "" {
		c.CredentialStore.Backend = "aesgcm"
	}
	if c.CredentialStore.Vault.Mount == "" {
		c.CredentialStore.Vault.Mount = "transit"
	}
	if c.CredentialStore.Vault.TokenEnv == "" {
		c.CredentialStore.Vault.TokenEnv = "VAULT_TOKEN"
	}
	if c.CredentialStore.Vault.Timeout <= 0 {
		c.CredentialStore.Vault.Timeout = 10 * time.Second
	}
	if c.CredentialStore.Vault.CacheTTL <= 0 {
		c.CredentialStore.Vault.CacheTTL = 30 * time.Second
	}
	// Registration 子配置默认（Web/CLI session TTL、per-AK 失败锁定阈值/窗口）。
	if c.Registration.SessionTTL <= 0 {
		c.Registration.SessionTTL = 24 * time.Hour
	}
	if c.Registration.CliTTL <= 0 {
		c.Registration.CliTTL = 7 * 24 * time.Hour
	}
	if c.Registration.LoginFailLimit <= 0 {
		c.Registration.LoginFailLimit = 5
	}
	if c.Registration.LoginFailWindow <= 0 {
		c.Registration.LoginFailWindow = 15 * time.Minute
	}
	if c.Sync.MaxConcurrent <= 0 {
		c.Sync.MaxConcurrent = 3
	}
	if c.Sync.TaskTTL <= 0 {
		c.Sync.TaskTTL = 24 * time.Hour
	}
	if c.Sync.MaxRetries < 1 {
		c.Sync.MaxRetries = 10
	}
	if c.Sync.RetryDelay <= 0 {
		c.Sync.RetryDelay = 10 * time.Second
	}
	if c.Sync.RetryBackoff <= 0 {
		c.Sync.RetryBackoff = 2
	}
	if c.Hub.MaxConnections <= 0 {
		c.Hub.MaxConnections = 256
	}
	if c.Hub.VirtualSubnet == "" {
		c.Hub.VirtualSubnet = hub.DefaultVirtualSubnet
	}
	if c.Hub.Transports.TCP.Enabled && c.Hub.Transports.TCP.Listen == "" {
		c.Hub.Transports.TCP.Listen = DefaultHubTCPListen
	}
	if c.Hub.Transports.XferTLS.Enabled && c.Hub.Transports.XferTLS.Listen == "" {
		c.Hub.Transports.XferTLS.Listen = DefaultXferTLSListen
	}
	if c.Hub.Transports.XferTCP.Enabled && c.Hub.Transports.XferTCP.Listen == "" {
		c.Hub.Transports.XferTCP.Listen = DefaultXferTCPListen
	}
	if c.Hub.Federation.Interval <= 0 {
		c.Hub.Federation.Interval = 30 * time.Second
	}
	if c.Hub.Federation.Timeout <= 0 {
		c.Hub.Federation.Timeout = 10 * time.Second
	}
	// audit.buffer_size 默认由 Default() 提供（2048）。此处**不**用 ==0 复活默认——
	// 加载链是 Default()（含 2048）→ Unmarshal → SetDefaults()，若这里有 ==0→2048，
	// 用户显式写的 audit.buffer_size: 0（=关闭）会被改回 2048，「0=关闭」不可达。
	// 装配侧（RegisterRoutes）按 >0 判断天然把 0 视为关闭，SetDefaults 无需兜底。
}

// Validate 校验配置合理性。
func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr 为空，请配置监听地址")
	}
	if c.StorageRoot == "" {
		return fmt.Errorf("storage_root 为空，请配置存储根目录")
	}
	// 兜底归一（幂等，仅空值时）：Validate 可能在 Normalize（SetDefaults）前被调
	// （如直接构造 &Config{...}.Validate()）——volumes 空视为未配合成单默认卷
	// （name=default, root=StorageRoot）、placement 空归一 prefer-default，保证后续
	// 卷校验与消费侧总能看到非空 Volumes 与合法 placement（假定或自行归一，两种路径均
	// 得校验）。非空值（含用户显式非法值）不被改写，交由下方校验拒绝。
	if len(c.Volumes) == 0 {
		c.Volumes = []VolumeConfig{{Name: "default", Root: c.StorageRoot}}
	}
	if c.Placement == "" {
		c.Placement = "prefer-default"
	}
	// volumes/placement 校验（多卷）。卷名复用 storage.ValidSegmentName 段名规则
	// （拒绝空/绝对/..、.__ 魔法前缀、Windows 保留名与非法字符），与租户/桶段名校验
	// 同一权威。
	switch c.Placement {
	case "prefer-default", "spread":
	default:
		return fmt.Errorf("placement 非法 %q：仅支持 prefer-default|spread", c.Placement)
	}
	seen := make(map[string]bool, len(c.Volumes))
	for i := range c.Volumes {
		v := &c.Volumes[i]
		if !storage.ValidSegmentName(v.Name) {
			return fmt.Errorf("卷名 %q 非法（拒绝空/绝对/..、.__ 前缀、Windows 保留名与非法字符）", v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("卷名重复 %q", v.Name)
		}
		seen[v.Name] = true
		if v.Root == "" {
			return fmt.Errorf("卷 %q root 为空（非首卷需显式指定挂载根）", v.Name)
		}
		if v.VolCapacity < 0 {
			return fmt.Errorf("卷 %q 容量上限 %d 非法：不能为负", v.Name, v.VolCapacity)
		}
		if a := v.ACL; a != nil {
			if a.Mode != VolumeACLAllow && a.Mode != VolumeACLDeny {
				return fmt.Errorf("卷 %q acl mode %q 非法：仅支持 allow|deny", v.Name, a.Mode)
			}
			for _, o := range a.Owners {
				if !storage.ValidSegmentName(o) {
					return fmt.Errorf("卷 %q acl owners 含非法 owner %q", v.Name, o)
				}
			}
		}
	}
	// audit.buffer_size 不能为负（0 = 关闭，正整数 = 环形容量）。
	if c.Audit.BufferSize < 0 {
		return fmt.Errorf("audit.buffer_size 不能为负，当前 %d（0=关闭，正整数=环形缓冲容量）", c.Audit.BufferSize)
	}
	// 无 auth 配置（凭据 Ring / api_keys 均空）在 Validate 层是合法的——
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
	// registration 子配置校验（TOTP 登录会话/锁定参数）：login_fail_limit 必须 ≥1
	// （<1 无法锁定，防脚枪）；session_ttl / cli_ttl / login_fail_window 必须 >0。
	if c.Registration.LoginFailLimit < 1 {
		return fmt.Errorf("registration.login_fail_limit=%d 无效，至少为 1（per-AK 失败锁定阈值）", c.Registration.LoginFailLimit)
	}
	if c.Registration.SessionTTL <= 0 {
		return fmt.Errorf("registration.session_ttl=%s 无效，必须大于 0", c.Registration.SessionTTL)
	}
	if c.Registration.CliTTL <= 0 {
		return fmt.Errorf("registration.cli_ttl=%s 无效，必须大于 0", c.Registration.CliTTL)
	}
	if c.Registration.LoginFailWindow <= 0 {
		return fmt.Errorf("registration.login_fail_window=%s 无效，必须大于 0", c.Registration.LoginFailWindow)
	}
	// bucket_limits 校验（任务 2 放行条件 2/3）：
	//   - 键是相对租户根路径（如 user/videos/hd），拒绝 .. / 绝对路径 / 前导斜杠 /
	//     空段 / 空串 / 尾部斜杠——防拼出越界或歧义路径子 Scope（quotaBucketFor 按
	//     filepath.ToSlash(path) 建键，装配期校验与消费保持一致语义）；
	//   - 与功能桶白名单（quotaBucketNames）重叠显式拒绝（fail-closed）：功能桶根上限
	//     恒 0（不单独限制，租户总上限单一执行）。若允许 "user:500" 覆盖功能桶子 Scope
	//     上限，装配顺序（先功能桶 0 后 BucketLimits 500）会做成"user 桶整体 500B 上限"，
	//     与"仅子目录限流"预期相反，且覆盖绕过静默不可查——配置即拒绝，防脚枪。
	for path, limit := range c.BucketLimits {
		n := strings.TrimSpace(path)
		bad := func() bool { // 键合法性：非空、非绝对（/ 或盘符）、非前导/尾部斜杠、无空段/.. 段
			if n == "" || strings.HasPrefix(n, "/") || strings.HasSuffix(n, "/") {
				return true
			}
			if len(n) >= 2 && n[1] == ':' {
				return true // Windows 盘符（C:/x、C:foo）视为绝对路径
			}
			if strings.Contains(n, `\`) {
				return true // 协议路径键恒用 /，拒绝反斜杠（避免跨平台歧义）
			}
			for seg := range strings.SplitSeq(n, "/") {
				if seg == "" || seg == "." || seg == ".." {
					return true
				}
			}
			return false
		}
		if bad() {
			return fmt.Errorf("bucket_limits 键 %q 非法：必须为相对租户根路径（如 user/videos/hd），不允许空、前导/尾部斜杠或 .. 段", path)
		}
		if !storage.ValidSegmentName(segNameOfBucketPath(n)) {
			return fmt.Errorf("bucket_limits 键 %q 含非法段（拒绝空/绝对/..、.__ 魔法前缀、Windows 保留名与非法字符）", path)
		}
		if slices.Contains(quotaBucketNames, n) {
			return fmt.Errorf("bucket_limits 键 %q 与功能桶根重叠：功能桶根上限由租户总 owner_quotas 单一执行，不支持单独 bucket_limits 覆盖", path)
		}
		// 键首段必须为 "user"（分层配额仅挂 user 桶 children 下）。cloud/archive/chunk/version
		// 桶内无用户子目录目录（archive/<name>、cloud/<taskID>），对它们配子目录永不生效，
		// 配置即拒绝防误导（fail-closed）。
		if first, _, ok := strings.Cut(n, "/"); !ok || first != "user" {
			return fmt.Errorf("bucket_limits 键 %q 非法：分层配额仅支持 user 桶子目录（如 user/videos/hd），其余功能桶无子目录结构", path)
		}
		if limit < 0 {
			return fmt.Errorf("bucket_limits[%q] 上限 %d 非法：配额上限不能为负", path, limit)
		}
	}
	if c.RateLimit.Enabled && c.RateLimit.Requests <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 requests=%d 无效，请设置大于 0 的值", c.RateLimit.Requests)
	}
	if c.RateLimit.Enabled && c.RateLimit.Window <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 window=%s 无效，请设置大于 0 的 duration", c.RateLimit.Window)
	}
	// telemetry 装配校验：仅 telemetry.enabled=true 时校验采样率与显式 OTLP 端点。
	// 采样率必须 ∈ (0,1]（ParentBased(TraceIDRatioBased) 合法输入）；显式
	// otlp_endpoint 必须为 http(s) 且带 host（空 = 仅走标准环境变量，合法）。
	if c.Telemetry.Enabled {
		if c.Telemetry.SampleRatio <= 0 || c.Telemetry.SampleRatio > 1 {
			return fmt.Errorf("telemetry.sample_ratio 必须 ∈ (0,1]，当前 %v", c.Telemetry.SampleRatio)
		}
		if c.Telemetry.OTLPEndpoint != "" {
			u, perr := url.Parse(c.Telemetry.OTLPEndpoint)
			if perr != nil {
				return fmt.Errorf("telemetry.otlp_endpoint %q 非法: %v", c.Telemetry.OTLPEndpoint, perr)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("telemetry.otlp_endpoint scheme %q 无效，仅允许 http/https", u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("telemetry.otlp_endpoint 缺少 host: %q", c.Telemetry.OTLPEndpoint)
			}
		}
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
	if c.Hub.VirtualSubnet != "" {
		// 虚拟 IP 分配仅支持 IPv4（确定性分配与递增分配均做 IPv4 算术）。非法/非 IPv4
		// CIDR 在启动时拒绝，防止分配器构造时 panic 或产生不可路由地址（M-3）。
		prefix, perr := netip.ParsePrefix(c.Hub.VirtualSubnet)
		if perr != nil {
			return fmt.Errorf("hub.virtual_subnet=%q 非法: %v", c.Hub.VirtualSubnet, perr)
		}
		if !prefix.Addr().Is4() {
			return fmt.Errorf("hub.virtual_subnet=%q 必须是 IPv4 CIDR（虚拟 IP 分配仅支持 IPv4）", c.Hub.VirtualSubnet)
		}
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
				// 与凭据 SK 的校验一致：SK 必须为 64 hex（32 字节 HMAC 密钥源）。
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
	// 允许配置空凭据的 remote 供未登记凭据的远程节点使用，创建任务时才拒绝）。
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
		// 明文 http 仅限 loopback（本机调试）：远程 remote 用 http 会把 SproxySig
		// AK/SK 明文上线，对齐联邦 peering 的 TLS 安全边界（安全审查 MEDIUM）。
		if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("sync_remotes[%d].url 使用明文 http 且非 loopback（AK/SK 将明文上线；远程 remote 请用 https，本机调试可用 http://127.0.0.1）: %q", i, r.URL)
		}
	}
	// credential_store 加密装配校验（4C-2 / Vault Transit）：先校验 backend 枚举，再按
	// backend 分支校验 Encrypt=true 的密钥来源——
	//   - aesgcm（缺省/空）：必须能解析出 master key（master_key_file 非空，文件可读性由
	//     装配层校验，见 BootstrapServerCredentials；或环境变量 CredentialMasterKeyEnv 已设）；
	//     二者皆无 fail-fast（防误开加密后启动即解密失败、用空凭据表运行）。
	//   - vault：要求 vault 子段 addr/key_name 与 token 源（token_file 或 TokenEnv 环境变量）
	//     齐全；忽略 master_key_file（Vault 持 key，无需本地 master key）。
	switch c.CredentialStore.Backend {
	case "", "aesgcm", "vault":
	default:
		return fmt.Errorf("credential_store.backend=%q 无效，仅允许 aesgcm 或 vault", c.CredentialStore.Backend)
	}
	if c.CredentialStore.Encrypt {
		switch c.CredentialStore.Backend {
		case "vault":
			if c.CredentialStore.Vault.Addr == "" {
				return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.addr")
			}
			// vault.addr 解析 + scheme 校验（对齐 sync_remotes 先例）。非 loopback 主机
			// 必须 https——http 明文传输 Vault token + 凭据属泄露向量（安全审查 MEDIUM）；
			// loopback 允许 http（dev 容器 http://127.0.0.1:8200）。
			u, perr := url.Parse(c.CredentialStore.Vault.Addr)
			if perr != nil {
				return fmt.Errorf("credential_store.vault.addr=%q 非法: %v", c.CredentialStore.Vault.Addr, perr)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("credential_store.vault.addr=%q scheme %q 非法，仅允许 http/https", c.CredentialStore.Vault.Addr, u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("credential_store.vault.addr=%q 缺少 host", c.CredentialStore.Vault.Addr)
			}
			if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
				return fmt.Errorf("credential_store.vault.addr=%q 非 loopback 必须使用 https（防 Vault token/凭据明文传输）", c.CredentialStore.Vault.Addr)
			}
			if c.CredentialStore.Vault.KeyName == "" {
				return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.key_name")
			}
			tokEnv := c.CredentialStore.Vault.TokenEnv
			if tokEnv == "" {
				tokEnv = "VAULT_TOKEN"
			}
			if c.CredentialStore.Vault.TokenFile == "" && os.Getenv(tokEnv) == "" {
				return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.token_file 或环境变量 %s", tokEnv)
			}
		default: // aesgcm / ""（向后兼容）
			if c.CredentialStore.MasterKeyFile == "" && os.Getenv(CredentialMasterKeyEnv) == "" {
				return fmt.Errorf("credential_store.encrypt=true 需配置 credential_store.master_key_file 或环境变量 %s（base64 编码 32B master key）", CredentialMasterKeyEnv)
			}
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
