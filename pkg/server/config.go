// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config.go 是服务端**配置的数据模型**：全部配置段结构体（TLS/ACME/限流/审计/OTEL/超时/版本/
// Hub/联邦/传输/Web/同步/注册/Vault/凭据库/卷 ACL/远程读写/Mesh/卷）、默认常量、以及顶层 Config
// 结构体与 OwnerQuotaFor。
//
// D3 拆分（2026-09-14）：本文件原为 1492 行单文件，按职责切为 4 个同包文件（**零 API 变更**，
// 纯代码搬迁，逐行比对已证）：config.go（本文件：数据模型与常量）、config_defaults.go（默认值）、
// config_validate.go（校验）、config_load.go（加载与保存）。
//
// 读源码的顺序建议：先本文件的 Config 结构体（这决定一份配置能写什么），再看
// config_defaults.go（没写时是什么）与 config_validate.go（写了什么会被拒绝）。

package server

import (
	"time"
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
// SyncRemoteConfig 是 `sync_remotes[]` 元素：一个同步远程节点。
//
// **载体分组**（kind 决定用哪组；缺省 direct = 旧配置零迁移）：
//   - direct：url + SproxySig 凭据（现状）；
//   - mesh：node + volume + peer_pins（零信任，无需对端可达；写批次装配）。
//
// 两组字段同时存在于本结构是刻意的：载体是「怎么到对端」的正交维度，模型一次定清，
// 将来加载体只加 kind 取值与参数，不改任务模型与持久化。
type SyncRemoteConfig struct {
	Name string `yaml:"name" mapstructure:"name"`
	// Kind 为载体类型："" / "direct"（默认）| "mesh"。
	Kind string `yaml:"kind" mapstructure:"kind"`

	// ---- direct 组 ----
	URL             string `yaml:"url" mapstructure:"url"`
	AccessKey       string `yaml:"access_key" mapstructure:"access_key"`
	AccessKeySecret string `yaml:"access_key_secret" mapstructure:"access_key_secret"`
	AccessKeyID     string `yaml:"access_key_id" mapstructure:"access_key_id"`

	// ---- mesh 组 ----
	Node      string   `yaml:"node" mapstructure:"node"`
	Volume    string   `yaml:"volume" mapstructure:"volume"`
	PeerPins  []string `yaml:"peer_pins" mapstructure:"peer_pins"`
	Transport string   `yaml:"transport" mapstructure:"transport"`
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
// Encrypt 缺省 false = 明文（现状 accesskey.CredentialStore，零回归）；true = 装配
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
	// MeshReaders 是跨节点授权条目（Y 一期只读 + Y 二期 scope 轴）；省略 = 无任何节点被授权。
	MeshReaders []VolumeMeshReaderConfig `yaml:"mesh_readers,omitempty" mapstructure:"mesh_readers"`
}

// VolumeMeshReaderConfig 是卷 ACL 的跨节点授权条目：把 mesh 节点身份（node + Ed25519 指纹）
// 绑定到一个 owner 命名空间与**权限范围 scope**（Y 二期 P3；规格 §5.7）。
//
// 安全边界：pkg/volume 侧的比较归一化**不做**规范形校验（不校验 sha256: 前缀与 64 位 hex），
// 畸形指纹在授权判定里只会「静默永不命中」；因此解析期（Validate）的校验是唯一防线，
// 非法指纹必须在此被响亮拒绝（fail-closed）。scope 同理（未知值必须响亮拒绝，而不是留到
// 运行期 fail-closed 静默拒绝——那会让「我明明配了写权限」无从排查）。
type VolumeMeshReaderConfig struct {
	Node        string `yaml:"node" mapstructure:"node"`
	Fingerprint string `yaml:"fingerprint" mapstructure:"fingerprint"`
	Owner       string `yaml:"owner" mapstructure:"owner"`
	// Scope 是授权范围：read（只读，**缺省**，零回归）| write（只写）| rw（读写）。
	// **读不隐含写、写不隐含读**；未知值在 Validate 期被拒绝。取值集合与判定实现单一事实源在
	// pkg/volume（volume.NormalizeMeshScope）。
	Scope string `yaml:"scope,omitempty" mapstructure:"scope"`
}

// RemoteReadConfig 是跨节点只读访问（Y 一期）的服务端配置（remote_read 段）。
//
// 只读面是 B 侧把「本节点被授权读的 owner 命名空间」经 mesh 暴露给对端节点的入口，
// 默认关闭（零回归：不配置则完全不起监听）。
type RemoteReadConfig struct {
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
	// Listen 是只读面监听地址；**强制 loopback**（配非 loopback 启动即失败，防被
	// 用作开放 mesh 中继——与既有网关安全边界同构：远程访问应经 mesh 而非直连）。
	Listen string `yaml:"listen" mapstructure:"listen"`
	// HandshakeTimeout 是隧道握手超时（透传 tunnel.WithHandshakeTimeout）。远程只读面
	// 的对端一建连即握手，无久等场景，故远小于 Tunnel 默认的 30s。
	HandshakeTimeout time.Duration `yaml:"handshake_timeout" mapstructure:"handshake_timeout"`
}

// RemoteWriteConfig 是跨节点写面（Y 二期 P3-b）的服务端配置（remote_write 段）。
//
// 与只读面**分开开关与监听**：写面是更高风险的暴露面（可改对端数据），运维必须能单独关闭它
// 而不影响只读同步，也必须能在与只读不同的地址上暴露（例如只把写面绑到另一张网卡）。
type RemoteWriteConfig struct {
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
	// Listen 是写面监听地址；**强制 loopback**（同只读面：远程访问应经 mesh 而非直连）。
	Listen string `yaml:"listen" mapstructure:"listen"`
	// HandshakeTimeout 是隧道握手超时（透传 tunnel.WithHandshakeTimeout）。
	HandshakeTimeout time.Duration `yaml:"handshake_timeout" mapstructure:"handshake_timeout"`
}

// MeshConfig 是**A 侧 mesh 客户端**配置（`kind=mesh` 的同步远端消费；Y 二期）。
//
// 与前几片的呼应：`sync_remotes[].kind=mesh` 决定「有哪些 mesh 远端」；本段决定「A 侧怎么接触
// hub 与信令」。**hub 可以是远端**（不必是本机）——这是本期明确支持的能力：
//
//   - `hub_url` 留空 ⇒ 用**本机 hub**（自连：服务发现/中继/信令都打本机 HTTP 面），凭据取本机
//     自用凭据（`Handlers.SelfCredential`），无需在此配置；
//   - `hub_url` 非空 ⇒ 视为**远端 hub**，必须配 `access_key`/`access_key_secret`/`skey_id`
//     （缺失会在 Validate 期响亮拒绝：空凭据只会得到 401，且要到任务运行期才暴露，无从排障）。
//
// `node_id` 是信令对端识别用的本节点 ID：**WebRTC 打洞（`transport: webrtc`）必需**；
// 留空 ⇒ 无信令 ⇒ `transport: auto` 退化为「纯中继」（可用但无打洞）。
type MeshConfig struct {
	// HubURL 是 hub 的 HTTP(S) base URL（如 https://hub.example.com:18083）；空 = 本机 hub。
	HubURL string `yaml:"hub_url,omitempty" mapstructure:"hub_url"`
	// NodeID 是本节点在 mesh 中的 ID（信令用；空 = 不启用信令 ⇒ 只能中继）。
	NodeID string `yaml:"node_id,omitempty" mapstructure:"node_id"`
	// AccessKey / AccessKeySecret / SkeyID 是访问（远端）hub 的 SproxySig 凭据。
	AccessKey       string `yaml:"access_key,omitempty" mapstructure:"access_key"`
	AccessKeySecret string `yaml:"access_key_secret,omitempty" mapstructure:"access_key_secret"`
	SkeyID          string `yaml:"skey_id,omitempty" mapstructure:"skey_id"`
	// InsecureTLS 允许对自签证书的远端 hub 跳过证书校验（**仅**开发/内网自签场景）。
	InsecureTLS bool `yaml:"insecure_tls,omitempty" mapstructure:"insecure_tls"`
	// STUN / TURN / TURNUser / TURNPassword 是**实例级** ICE 配置（走 webrtc.ICEOptions，
	// **不污染** CLI 侧的包级全局）：本进程内所有 mesh 载体的打洞都用这一份。
	STUN         []string `yaml:"stun,omitempty" mapstructure:"stun"`
	TURN         []string `yaml:"turn,omitempty" mapstructure:"turn"`
	TURNUser     string   `yaml:"turn_user,omitempty" mapstructure:"turn_user"`
	TURNPassword string   `yaml:"turn_password,omitempty" mapstructure:"turn_password"`
	// Node 是 B 侧 mesh node 角色（可选；启用后无需外部 sidecar）。
	Node MeshNodeConfig `yaml:"node,omitempty" mapstructure:"node"`
}

// MeshNodeID 返回 mesh node 角色使用的节点 ID（node_id 优先，回落 hub.node_id）。
func (c *Config) MeshNodeID() string {
	if c.Mesh.Node.NodeID != "" {
		return c.Mesh.Node.NodeID
	}
	if c.Mesh.NodeID != "" {
		return c.Mesh.NodeID
	}
	return c.Hub.NodeID
}

// MeshNodeHubURL 返回 mesh node 角色注册用的 hub 地址（node.hub_url → mesh.hub_url → 空 = 本机）。
func (c *Config) MeshNodeHubURL() string {
	if c.Mesh.Node.HubURL != "" {
		return c.Mesh.Node.HubURL
	}
	return c.Mesh.HubURL
}

// MeshNodeConfig 是 **B 侧 mesh node 角色**配置（`mesh.node` 段；S5）。
//
// 背景：B 侧要把本机 `remote_read`/`remote_write` 面宣告到 mesh（供对端 A 经服务发现 + 出口拨号
// 到达），此前依赖**外部 sidecar**（`sclient mesh node --service volread:… --dial-allow`）。
// 启用本段后由 sproxy 进程自身承担该角色（`mesh.RunNode`），部署形态从「sproxy + sidecar」收敛为
// 「sproxy」；不启用则与今天完全一致（零回归）。
type MeshNodeConfig struct {
	// Enabled 是否在服务端进程内运行 mesh node 角色（默认 false = 走外部 sidecar）。
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
	// HubURL 是注册用的 hub 地址（http(s)/ws(s)）；空 = 回落 `mesh.hub_url`，再空 = 本机 hub
	// （由 `cfg.Addr`+TLS 派生）。
	HubURL string `yaml:"hub_url,omitempty" mapstructure:"hub_url"`
	// NodeID 是本节点在 mesh 中的稳定 ID；空 = 回落 `mesh.node_id`，再空 = `hub.node_id`。
	NodeID string `yaml:"node_id,omitempty" mapstructure:"node_id"`
	// WebRTC 是否接受 WebRTC 直连（信令 poll + listen）；默认 false = 只提供 hub 中继
	// （保守默认：直连需要在 hub 上做信令交换）。
	WebRTC bool `yaml:"webrtc,omitempty" mapstructure:"webrtc"`
	// Insecure 对自签证书的（WSS）hub 跳过证书校验（仅开发/内网）。
	Insecure bool `yaml:"insecure,omitempty" mapstructure:"insecure"`
	// DialAllowCIDRs 是出口拨号额外放行的网段；`remote_read`/`remote_write` 的 loopback 地址
	// 已由服务宣告**自动精确放行**，无需在此重复。
	DialAllowCIDRs []string `yaml:"dial_allow_cidrs,omitempty" mapstructure:"dial_allow_cidrs"`
	// ExtraServices 是额外宣告的服务（`name:host:port`，可多次）；缺省自动宣告
	// `volread`/`volwrite`（按 `remote_read`/`remote_write` 的监听地址）。
	ExtraServices []string `yaml:"extra_services,omitempty" mapstructure:"extra_services"`
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

	// RemoteRead 是跨节点只读访问（Y 一期）的服务端配置（默认关闭）。
	RemoteRead RemoteReadConfig `yaml:"remote_read" mapstructure:"remote_read"`

	// RemoteWrite 是跨节点写访问（Y 二期 P3-b）的服务端配置（默认关闭；与只读面独立开关/监听）。
	RemoteWrite RemoteWriteConfig `yaml:"remote_write" mapstructure:"remote_write"`

	// Mesh 是 A 侧 mesh 客户端配置（`kind=mesh` 的同步远端消费；hub 可为远端）。
	Mesh MeshConfig `yaml:"mesh" mapstructure:"mesh"`

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
