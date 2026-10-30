// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config_alias.go 是**迁移期兼容层**：配置数据模型已下沉到 pkg/server/config
// （server 作用域），本文件用类型别名 / 常量别名 / 一行转发让存量代码零改动。
//
// 为什么需要它：`Config` 及其 50+ 嵌套类型被 pkg/server 的 100+ 文件与 cmd/sproxy
// 直接引用；一次性改全部调用点会把「搬迁」和「语义变更」混在一个 PR 里，无法 review。
//
// **移除条件**：pkg/server 拆分为 pkg/server/<cap> 子包完成后，各子包直接 import
// pkg/server/config；届时删除本文件，并把 cmd/sproxy 的引用改为 config.*。
// 在此之前，**新代码请直接使用 pkg/server/config，不要往本文件加新别名**。
package server

import (
	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/server/config"
)

// ---- 类型别名（零成本，方法与字段随原类型）----
type (
	ACMEConfig              = config.ACMEConfig
	ArchiveConfig           = config.ArchiveConfig
	AuditConfig             = config.AuditConfig
	AuditStartupConfig      = config.AuditStartupConfig
	AuthConfig              = config.AuthConfig
	AuthExternalConfig      = config.AuthExternalConfig
	BandwidthConfig         = config.BandwidthConfig
	ByteSize                = config.ByteSize
	Config                  = config.Config
	CredentialStoreConfig   = config.CredentialStoreConfig
	CredentialsConfig       = config.CredentialsConfig
	DedupConfig             = config.DedupConfig
	FederationConfig        = config.FederationConfig
	FederationPeerConfig    = config.FederationPeerConfig
	GRPCTransportConfig     = config.GRPCTransportConfig
	HubConfig               = config.HubConfig
	LDAPConfig              = config.LDAPConfig
	MaintenanceWindowConfig = config.MaintenanceWindowConfig
	MeshConfig              = config.MeshConfig
	MeshNodeConfig          = config.MeshNodeConfig
	OIDCConfig              = config.OIDCConfig
	OTELConfig              = config.OTELConfig
	PikpakAccountConfig     = config.PikpakAccountConfig
	PikpakConfig            = config.PikpakConfig
	PikpakHybridConfig      = config.PikpakHybridConfig
	QUICTransportConfig     = config.QUICTransportConfig
	RateLimitConfig         = config.RateLimitConfig
	RegistrationConfig      = config.RegistrationConfig
	RemoteReadConfig        = config.RemoteReadConfig
	RemoteWriteConfig       = config.RemoteWriteConfig
	RotationConfig          = config.RotationConfig
	SchedulerConfig         = config.SchedulerConfig
	ServerTimeouts          = config.ServerTimeouts
	SyncConfig              = config.SyncConfig
	SyncRemoteConfig        = config.SyncRemoteConfig
	TCPTransportConfig      = config.TCPTransportConfig
	TLSConfig               = config.TLSConfig
	TierPolicyConfig        = config.TierPolicyConfig
	TransportConfigs        = config.TransportConfigs
	TrashConfig             = config.TrashConfig
	TrustedVolumeConfig     = config.TrustedVolumeConfig
	UsageConfig             = config.UsageConfig
	VaultConfig             = config.VaultConfig
	VersionConfig           = config.VersionConfig
	VolumeACLConfig         = config.VolumeACLConfig
	VolumeACLMode           = config.VolumeACLMode
	VolumeConfig            = config.VolumeConfig
	VolumeMeshReaderConfig  = config.VolumeMeshReaderConfig
	VolumeRetentionConfig   = config.VolumeRetentionConfig
	VolumeStateConfig       = config.VolumeStateConfig
	WSTransportConfig       = config.WSTransportConfig
	WebConfig               = config.WebConfig
	WebDAVConfig            = config.WebDAVConfig
	XferTransportConfig     = config.XferTransportConfig
)

// ---- 常量别名 ----
const (
	CredentialMasterKeyEnv = config.CredentialMasterKeyEnv
	DefaultHubGRPCListen   = config.DefaultHubGRPCListen
	DefaultHubQUICListen   = config.DefaultHubQUICListen
	DefaultHubTCPListen    = config.DefaultHubTCPListen
	DefaultXferTCPListen   = config.DefaultXferTCPListen
	DefaultXferTLSListen   = config.DefaultXferTLSListen
	VolumeACLAllow         = config.VolumeACLAllow
	VolumeACLDeny          = config.VolumeACLDeny

	// defaultStorageRoot 保留小写名：包内多处直接引用（routes/volumes），
	// 与 config.DefaultStorageRoot 同源（常量别名，无第二份取值）。
	defaultStorageRoot = config.DefaultStorageRoot
)

// ---- 函数转发（仅 4 个导出入口；其余函数/方法随类型走）----

// Default 返回内存基线配置（迁移期转发，见文件头）。
func Default() *Config { return config.Default() }

// LoadConfig 从 YAML 文件加载配置（迁移期转发）。
func LoadConfig(path string) (*Config, error) { return config.LoadConfig(path) }

// SaveConfig 原子写回 YAML 配置（迁移期转发）。
func SaveConfig(cfg *Config, path string) error { return config.SaveConfig(cfg, path) }

// LoadFromProvider 从配置提供者加载（迁移期转发）。
func LoadFromProvider(p provider.Provider) (*Config, error) { return config.LoadFromProvider(p) }

// ---- 第二批：随 config 切片一并下沉的配置类型（notify/ai/cluster/alerts/auth/cors/ratelimit）----
type (
	AIAdvisorConfig         = config.AIAdvisorConfig
	AIEventsConfig          = config.AIEventsConfig
	AIInsightConfig         = config.AIInsightConfig
	AILimit                 = config.AILimit
	AIPrivacyConfig         = config.AIPrivacyConfig
	AIQuotaConfig           = config.AIQuotaConfig
	AISearchConfig          = config.AISearchConfig
	APIKey                  = config.APIKey
	APIKeyConfig            = config.APIKeyConfig
	AlertConfig             = config.AlertConfig
	AlertRule               = config.AlertRule
	AlertmanagerConfig      = config.AlertmanagerConfig
	CORSConfig              = config.CORSConfig
	ClusterConfig           = config.ClusterConfig
	ClusterCredentialConfig = config.ClusterCredentialConfig
	EmailConfig             = config.EmailConfig
	EndpointLimit           = config.EndpointLimit
	GrafanaConfig           = config.GrafanaConfig
	MongoConfig             = config.MongoConfig
	NotifyChannelsConfig    = config.NotifyChannelsConfig
	NotifyConfig            = config.NotifyConfig
	NotifyRule              = config.NotifyRule
	ServerChanConfig        = config.ServerChanConfig
	StateStoreConfig        = config.StateStoreConfig
	WebhookConfig           = config.WebhookConfig
	WecomConfig             = config.WecomConfig
)

const (
	// PermissionRead / PermissionWrite 是 API Key 权限取值（与 config 同源）。
	PermissionRead  = config.PermissionRead
	PermissionWrite = config.PermissionWrite
	// DefaultMaxAge 是 CORS 预检默认缓存秒数（与 config 同源）。
	DefaultMaxAge = config.DefaultMaxAge
	// 集群节点角色（与 config 同源）。
	ClusterRoleMaster  = config.ClusterRoleMaster
	ClusterRoleReplica = config.ClusterRoleReplica
)
