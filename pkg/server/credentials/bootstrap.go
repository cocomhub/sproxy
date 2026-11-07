// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package credentials 承载凭据的**存储后端与主密钥解析**：
//   - 凭据 Ring 的快照存储（StateStore 后端 + 旧 meta 文件双读单写、可选静态加密）；
//   - 生产装配入口 BootstrapServerCredentials（store 选型 + 加密包装 + 载入 Ring）；
//   - 主密钥 / Vault token 来源解析（master_key_file > 环境变量 > Vault）；
//   - Ring 首个存活凭据选取（BestFirstCredential / BestCredential）。
//
// 边界：本包只做**存储与密钥解析**，不持有 `*Handlers`、不反向依赖装配根
// `pkg/server`（R6）。启动编排（`(h *Handlers) bootstrapCredentials`、零凭据启动日志与
// panic 语义）、自用凭据自省（`SelfCredential`）、凭据轮换循环（renew/prune/持久化接线）
// 都依赖装配态或装配 opts，仍留在装配层。
//
// 行为不变量（启动路径，逐字保持）：master key 来源顺序（master_key_file → 环境变量
// CredentialMasterKeyEnv）、Vault token 来源顺序（token_file → TokenEnv）、AAD 推导
// （owner 唯一相对 storage_root 路径的 ToSlash 形态）、加密开关语义与全部错误文案。
package credentials

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// credentialRingKey 是凭据 Ring 在 StateStore 中的 key（statestore.md §2.3 三段式
// <owner>/<type>/<name>；credential 迁移矩阵 P0，见 §5.1）。
const credentialRingKey = "credential/anonymous/ring"

// credentialsFileName 是凭据快照的落盘文件名（旧路径 <默认卷根>/anonymous/meta/ 下的
// 文件名；StateStore 后端的回退读路径也用它）。
const credentialsFileName = "credentials.json"

// BootstrapServerCredentials 是生产装配入口：为服务端准备凭据 Ring + store
// （供 cmd/sproxy 在 RegisterRoutes 与 hub 装配之前调用，随后把二者注入 opts）。
//   - 单节点默认（无 state_store 段 / 显式 local + cluster 关）：store = <默认卷根>
//     /anonymous/meta/credentials.json（服务端级全局凭据，anonymous 租户的 meta 桶；
//     多租户部署如需 per-owner 凭据经 /api/credentials 管理，见任务 5）。
//     **默认卷根经 defaultVolumeRoot 裁决**（非 cfg.StorageRoot）——显式
//     volumes[0].root ≠ storage_root 分叉时凭据必须落默认卷 meta（AD-5 meta 归属默认卷
//     不变式；否则重启凭据 Ring 丢失，PR-B 终审建议 9）。
//   - 集群模式（cluster.enabled=true 或 state_store.type != local）→ store 切 StateStore
//     后端（stateBackedCredentialStore，StateStore.Get/Put，key=credential/anonymous/ring），
//     读旧 meta 回退 + 首写迁新路径（双读单写零回归）；StateStore 装配失败 → 启动失败
//     （fail-closed，防「以为多节点一致、实际各写各的」）。
//   - 载入既有快照；**U3：不再生成首启 anonymous 凭据**——store 为空则返回空 Ring，
//     系统以零凭据等待 register 公开端点（首个回环注册者授 admin）。
func BootstrapServerCredentials(cfg *config.Config, logger *slog.Logger) (*accesskey.Ring, accesskey.CredentialStorer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	metaDir := filepath.Join(defaultVolumeRoot(cfg), storage.AnonymousOwner, "meta")
	legacyPath := filepath.Join(metaDir, credentialsFileName)
	// 集群模式派生（cluster-state-migration.md §2.1）：state_store.type != local（显式
	// 共享）或 cluster.enabled（共享外部卷形态）→ StateStore 后端；其余单节点零回归。
	stateBacked := cfg.StateStore.Type != "" && cfg.StateStore.Type != "local" || cfg.Cluster.Enabled
	var st state.StateStore
	if stateBacked {
		var serr error
		st, serr = newStateBackedStore(cfg, logger)
		if serr != nil {
			return nil, nil, serr
		}
	}
	var store accesskey.CredentialStorer
	if stateBacked {
		store = newStateBackedCredentialStore(st, credentialRingKey, legacyPath, nil)
	} else {
		store = accesskey.NewCredentialStore(metaDir)
	}
	// 4C-2：credential_store.encrypt=true 时把凭据文件包装为加密静态存储
	// （EncryptingStorer，按 backend 选 SecureStorer）——cmd 与 opts 注入面不变
	// （返回类型已是 CredentialStorer 接口，替换实现无缝）。默认关 = 明文零回归。
	// backend：aesgcm（缺省/空）= 本地 AES-256-GCM master key；vault = Vault Transit
	// （密钥不出 Vault）。token/凭据不落日志。
	if cfg.CredentialStore.Encrypt {
		encStore, err := buildEncryptedCredentialStore(cfg, stateBacked, st, metaDir, legacyPath, logger)
		if err != nil {
			return nil, nil, err
		}
		store = encStore
	}
	ring, err := loadRingFromStore(store, logger)
	if err != nil {
		return nil, nil, err
	}
	return ring, store, nil
}

// defaultVolumeRoot 裁决默认卷（Volumes[0]）的物理挂载根，与装配层
// `pkg/server.resolveDefaultVolumeRoot` 的 F1 规则同义（该函数是规则的事实源，见
// `pkg/server/volumes.go`）：首卷 root 仍是占位形态（== config.DefaultStorageRoot，
// 即用户未显式给首卷 root）时用 cfg.StorageRoot，否则用首卷显式 root。
//
// 为什么不直接复用装配层函数：本包不得反向 import `pkg/server`（R6）。两处取值一致
// 由行为测试锁定——`pkg/server/feature_bucket_volume_test.go` 的分叉配置用例断言凭据
// 落在**默认卷根**而非 cfg.StorageRoot。
func defaultVolumeRoot(cfg *config.Config) string {
	if len(cfg.Volumes) == 0 {
		return cfg.StorageRoot
	}
	if cfg.Volumes[0].Root == config.DefaultStorageRoot {
		return cfg.StorageRoot
	}
	return cfg.Volumes[0].Root
}

// newStateBackedStore 装配 StateStore 后端（集群模式凭据必选 StateStore）。失败（type/dir
// 配置非法或后端不可达）返回错误——fail-closed，防「以为多节点一致、实际各写各的」。
func newStateBackedStore(cfg *config.Config, logger *slog.Logger) (state.StateStore, error) {
	stateDir := cfg.StateStore.Dir
	if stateDir == "" {
		stateDir = filepath.Join(cfg.StorageRoot, "state")
	}
	st, serr := state.NewStateStore(cfg.StateStore.Type, state.StateStoreConfig{
		Type:  cfg.StateStore.Type,
		Dir:   stateDir,
		Mongo: state.MongoConfig{URI: cfg.StateStore.Mongo.URI, Database: cfg.StateStore.Mongo.Database, Collection: cfg.StateStore.Mongo.Collection},
	}, logger)
	if serr != nil {
		return nil, fmt.Errorf("装配 StateStore 失败（集群模式凭据必选 StateStore）: %w", serr)
	}
	logger.Info("凭据后端切换 StateStore", "type", cfg.StateStore.Type, "dir", stateDir)
	return st, nil
}

// buildEncryptedCredentialStore 按 credential_store.encrypt 把凭据 store 包装为加密静态
// 存储（aesgcm 本地 master key / vault Transit）。StateStore 后端时 secure 内嵌
// StateBackedCredentialStore 内层（StateStore 值为密文）。backend 是规范化后的日志值。
func buildEncryptedCredentialStore(cfg *config.Config, stateStore bool, st state.StateStore, metaDir, legacyPath string, logger *slog.Logger) (accesskey.CredentialStorer, error) {
	storePath := filepath.Join(metaDir, credentialsFileName)
	backend := "aesgcm"
	var secure accesskey.SecureStorer
	switch cfg.CredentialStore.Backend {
	case "vault":
		backend = "vault"
		tok, err := resolveVaultToken(cfg.CredentialStore.Vault)
		if err != nil {
			return nil, err
		}
		v, err := accesskey.NewVaultTransitStorer(accesskey.VaultOptions{
			Addr:     cfg.CredentialStore.Vault.Addr,
			Mount:    cfg.CredentialStore.Vault.Mount, // SetDefaults 已填 "transit"
			KeyName:  cfg.CredentialStore.Vault.KeyName,
			Token:    tok,
			CAFile:   cfg.CredentialStore.Vault.CAFile,
			Timeout:  cfg.CredentialStore.Vault.Timeout, // SetDefaults 已填 10s
			CacheTTL: cfg.CredentialStore.Vault.CacheTTL,
			// I-2：AAD context 绑 owner 唯一相对 storage_root 路径（匿名租户全局凭据
			// 文件）……filepath.ToSlash 归一跨平台路径分隔符，防 Windows 反斜杠导致 AAD 不一致。
			AADPath: filepath.ToSlash(filepath.Join(storage.AnonymousOwner, "meta", credentialsFileName)),
		})
		if err != nil {
			return nil, err
		}
		// 启动探活（F1）：POST /v1/auth/token/lookup-self 同时验可达性 + token 有效性。
		// 空 store 首启也探（Load 无密文不发 Vault 请求）——防配错 Vault 静默启动到首写才炸。
		if perr := v.Probe(); perr != nil {
			return nil, fmt.Errorf("credential_store.backend=vault 启动探活失败（可达性或 token 有效性）: %w", perr)
		}
		secure = v
	default: // aesgcm（含空 = 向后兼容）
		masterKey, err := ResolveCredentialMasterKey(cfg)
		if err != nil {
			return nil, err
		}
		secure = accesskey.AESGCMStorer{Key: masterKey}
	}
	var store accesskey.CredentialStorer
	if stateStore {
		// 加密链保留（cluster-state-migration.md §2.2）：secure 内嵌 StateBacked 内层，
		// StateStore 值为密文，Vault/aesgcm 语义与 EncryptingStorer 一致。
		store = newStateBackedCredentialStore(st, credentialRingKey, legacyPath, secure)
	} else {
		store = accesskey.NewEncryptingStorer(storePath, secure)
	}
	logger.Info("凭据静态存储加密已启用", "backend", backend)
	return store, nil
}

// loadRingFromStore 从凭据 store 载入快照重建 Ring（损坏 → fail-closed error；U3：空
// store 不生成 anonymous，返回空 Ring 等待 register 公开端点）。
func loadRingFromStore(store accesskey.CredentialStorer, logger *slog.Logger) (*accesskey.Ring, error) {
	ring := accesskey.NewRing()
	if keys, err := store.Load(); err != nil {
		return nil, fmt.Errorf("载入凭据 store 失败（fail-closed）: %w", err)
	} else if len(keys) > 0 {
		if rerr := ring.Replace(keys); rerr != nil {
			return nil, fmt.Errorf("重建凭据 Ring 失败: %w", rerr)
		}
		logger.Info("已从凭据 store 载入", "keys", len(keys))
	}
	// U3：不生成 anonymous——空 store = 零凭据等待注册。
	if ring.Len() == 0 {
		logger.Info("零凭据启动：请在本机回环执行 /api/credentials/register，首个注册者将成为 admin")
	}
	return ring, nil
}
