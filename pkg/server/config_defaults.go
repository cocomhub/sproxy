// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config_defaults.go 是**默认值**：Default()（内存中的基线配置）、SetDefaults()（对已解码配置补齐
// 省略项，含从环境变量推导的项），以及联邦 Mesh 远程判定助手 hasKindMeshRemote。
//
// 拆分说明见 config.go 顶部。

package server

import (
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/volume"
)

func Default() *Config {
	return &Config{
		Addr:        ":18083",
		StorageRoot: defaultStorageRoot,
		// OwnerQuotas/BucketLimits 默认空 map（非 nil，便于 map 判断/访问复用）。
		OwnerQuotas:  map[string]ByteSize{},
		BucketLimits: map[string]ByteSize{},
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
		Credentials: CredentialsConfig{
			Rotation: RotationConfig{
				Interval:     0,                  // 默认关闭自动轮换（零回归）
				NotifyBefore: 7 * 24 * time.Hour, // 到期前 7 天开始轮换
				KeepOld:      2,                  // 保留旧 SK 数（宽限期）
			},
		},
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
		// 跨节点只读面（Y 一期）：默认关闭；listen 默认 loopback 高位端口供显式启用。
		RemoteRead: RemoteReadConfig{
			Enabled:          false,
			Listen:           "127.0.0.1:19000",
			HandshakeTimeout: 10 * time.Second,
		},
		RemoteWrite: RemoteWriteConfig{
			Enabled:          false,
			Listen:           "127.0.0.1:19001",
			HandshakeTimeout: 10 * time.Second,
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

// hasKindMeshRemote 报告是否配置了 `kind=mesh` 的同步远端（决定 mesh 段是否参与校验）。
func hasKindMeshRemote(c *Config) bool {
	for _, r := range c.SyncRemotes {
		if syncmgr.RemoteKind(r.Kind) == syncmgr.RemoteKindMesh {
			return true
		}
	}
	return false
}

// SetDefaults 设置零值字段为默认值。
func (c *Config) SetDefaults() {
	if c.Addr == "" {
		c.Addr = ":18083"
	}
	if c.StorageRoot == "" {
		c.StorageRoot = defaultStorageRoot
	}
	// max_upload_bytes 可配置（roadmap P0）：<=0 回落 1 GiB 默认（零回归）。
	// 取值上限：普通上传上限受分块单文件上界约束（65536 块 × 最大块），此处不重复设上限
	// （超大配置由运行时行为兜底：请求体超 MaxBytesReader 直接 413）。
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = ByteSize(size.UploadBodyLimit)
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
		// Y 一期：mesh_readers 指纹归一为规范形（去空白/大小写/可省前缀）。
		// 非法指纹在此保持原样，交由 Validate 响亮拒绝（fail-closed）。
		// ac 恒非 nil：上面几行的缺省填充（ACL == nil 即赋空 ACL）已建立该不变式，
		// 故此处不做 nil 比较（做了也是死分支，反而让人误以为 ACL 可为 nil）。
		ac := c.Volumes[i].ACL
		for j := range ac.MeshReaders {
			if norm, err := tunnel.ParseFingerprint(ac.MeshReaders[j].Fingerprint); err == nil {
				ac.MeshReaders[j].Fingerprint = norm
			}
			// Y 二期：scope 归一为规范小写形（未知值保持原样，交由 Validate 响亮拒绝）。
			if scope, ok := volume.NormalizeMeshScope(ac.MeshReaders[j].Scope); ok {
				ac.MeshReaders[j].Scope = scope
			}
		}
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = size.DefaultChunkSize
	}
	// 跨节点只读面（Y 一期）：零值兜底（viper 未配时不留 0 值，避免 HandshakeTimeout=0
	// 被 tunnel 当成「用默认 30s」以外的歧义语义）。
	if c.RemoteRead.Listen == "" {
		c.RemoteRead.Listen = "127.0.0.1:19000"
	}
	if c.RemoteRead.HandshakeTimeout <= 0 {
		c.RemoteRead.HandshakeTimeout = 10 * time.Second
	}
	// 跨节点写面（Y 二期）：零值兜底（与只读面同构；端口取 19001 避免与只读面冲突）。
	if c.RemoteWrite.Listen == "" {
		c.RemoteWrite.Listen = "127.0.0.1:19001"
	}
	if c.RemoteWrite.HandshakeTimeout <= 0 {
		c.RemoteWrite.HandshakeTimeout = 10 * time.Second
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
	// credentials.rotation 默认（2026-09-17）：interval 0=关闭；notify_before 7d；keep_old 2。
	// 与 Default() 一致（SetDefaults 对从 viper/YAML 载入的配置兜底）。
	if c.Credentials.Rotation.Interval == 0 {
		c.Credentials.Rotation.Interval = 0 // 显式 0 = 关闭（保持零回归）
	}
	if c.Credentials.Rotation.NotifyBefore == 0 {
		c.Credentials.Rotation.NotifyBefore = 7 * 24 * time.Hour
	}
	if c.Credentials.Rotation.KeepOld == 0 {
		c.Credentials.Rotation.KeepOld = 2
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
	if c.Hub.Transports.QUIC.Enabled && c.Hub.Transports.QUIC.Listen == "" {
		c.Hub.Transports.QUIC.Listen = DefaultHubQUICListen
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
