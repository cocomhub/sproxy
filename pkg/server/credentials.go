// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// credentials.go 是**凭据装配与自省**：bootstrapCredentials（启动时装配凭据 Ring 与关联 store，
// 含零凭据启动语义）、BootstrapServerCredentials（二进制启动入口）、凭据主密钥/Vault token
// 解析（resolveCredentialMasterKey / resolveVaultToken）、最佳凭据选取（bestFirstCredential /
// bestCredential），以及只暴露本进程自身凭据的 SelfCredential。
//
// 拆分说明见 handlers.go 顶部。

package server

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// bootstrapCredentials 装配凭据 Ring 与关联 store（RegisterRoutes 启动时调用一次）：
//   - 显式注入（opts.CredentialRing）→ 直接使用；
//   - 否则从 opts.CredentialStore 载入快照（真实/损坏处理见 CredentialStore.Load）；
//   - **U3：零凭据启动**——不再生成首启 anonymous 凭据（4A 的 generateBootstrapCredential
//     路径已移除）。store 为空 = 系统以零凭据等待注册：register 公开端点是唯一用户
//     入口，首个经回环注册的用户由 AddRegistration 原子授 admin（DEC-F/D2）。
//     空 store 时记启动日志提示「首次注册经回环，将成为 admin」（S2）。
func (h *Handlers) bootstrapCredentials(opts RegisterRoutesOpts) {
	store := normalizeStorer(opts.CredentialStore)
	if opts.CredentialRing != nil {
		h.credentialRing = opts.CredentialRing
		h.credentialStore = store
		return
	}
	ring := accesskey.NewRing()
	if store != nil {
		if keys, err := store.Load(); err != nil {
			h.logger.Error("载入凭据 store 失败（fail-closed：拒绝启动，防止用空凭据表运行）", "error", err)
			panic("载入凭据 store 失败: " + err.Error())
		} else if len(keys) > 0 {
			if rerr := ring.Replace(keys); rerr != nil {
				h.logger.Error("重建凭据 Ring 失败（fail-closed）", "error", rerr)
				panic("重建凭据 Ring 失败: " + rerr.Error())
			}
			h.logger.Info("已从凭据 store 载入", "keys", len(keys))
		}
	}
	// U3：零凭据等待注册——store 为空即不生成任何凭据（ring.Len()==0），
	// 首个注册者（回环）由 register 端点经 AddRegistration 原子授 admin。
	if ring.Len() == 0 {
		h.logger.Info("零凭据启动：首次注册请在本机回环执行 /api/credentials/register，首位注册者将成为 admin")
	}
	h.credentialRing = ring
	h.credentialStore = store
}

// BootstrapServerCredentials 是生产装配入口：为服务端准备凭据 Ring + store
// （供 cmd/sproxy 在 RegisterRoutes 与 hub 装配之前调用，随后把二者注入 opts）。
//   - store = <默认卷根>/anonymous/meta/credentials.json（服务端级全局凭据，anonymous
//     租户的 meta 桶；多租户部署如需 per-owner 凭据经 /api/credentials 管理，见任务 5）。
//     **默认卷根经 resolveDefaultVolumeRoot 裁决**（非 cfg.StorageRoot）——显式
//     volumes[0].root ≠ storage_root 分叉时凭据必须落默认卷 meta（AD-5 meta 归属默认卷
//     不变式；否则重启凭据 Ring 丢失，PR-B 终审建议 9）。
//   - 载入既有快照；**U3：不再生成首启 anonymous 凭据**——store 为空则返回空 Ring，
//     系统以零凭据等待 register 公开端点（首个回环注册者授 admin）。
func BootstrapServerCredentials(cfg *Config, logger *slog.Logger) (*accesskey.Ring, accesskey.CredentialStorer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	metaDir := filepath.Join(resolveDefaultVolumeRoot(cfg), anonymousOwner, "meta")
	var store accesskey.CredentialStorer = accesskey.NewCredentialStore(metaDir)
	// 4C-2：credential_store.encrypt=true 时把凭据文件包装为加密静态存储
	// （EncryptingStorer，按 backend 选 SecureStorer）——cmd 与 opts 注入面不变
	// （返回类型已是 CredentialStorer 接口，替换实现无缝）。默认关 = 明文零回归。
	// backend：aesgcm（缺省/空）= 本地 AES-256-GCM master key；vault = Vault Transit
	// （密钥不出 Vault）。token/凭据不落日志。
	if cfg.CredentialStore.Encrypt {
		storePath := filepath.Join(metaDir, "credentials.json")
		// backend 是规范化后的日志值：直接 Config{Backend:""}（未经 SetDefaults）走 aesgcm
		// 分支时 cfg.Backend 为空串，日志应仍记 "aesgcm"（M-1）。
		backend := "aesgcm"
		var secure accesskey.SecureStorer
		switch cfg.CredentialStore.Backend {
		case "vault":
			backend = "vault"
			tok, err := resolveVaultToken(cfg.CredentialStore.Vault)
			if err != nil {
				return nil, nil, err
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
				// 文件）——同 vault mount+key 下不同凭据文件 context 各不相同，密文被复制/
				// 搬移到另一文件即 decrypt 失败（防跨节点/租户搬移）。filepath.ToSlash 归一
				// 跨平台路径分隔符，防 Windows 反斜杠导致 AAD 不一致。
				AADPath: filepath.ToSlash(filepath.Join(anonymousOwner, "meta", "credentials.json")),
			})
			if err != nil {
				return nil, nil, err
			}
			// 启动探活（F1）：POST /v1/auth/token/lookup-self 同时验可达性 + token 有效性。
			// 空 store 首启也探（Load 无密文不发 Vault 请求）——防配错 Vault 静默启动到首写才炸。
			if perr := v.Probe(); perr != nil {
				return nil, nil, fmt.Errorf("credential_store.backend=vault 启动探活失败（可达性或 token 有效性）: %w", perr)
			}
			secure = v
		default: // aesgcm（含空 = 向后兼容）
			masterKey, err := resolveCredentialMasterKey(cfg)
			if err != nil {
				return nil, nil, err
			}
			secure = accesskey.AESGCMStorer{Key: masterKey}
		}
		store = accesskey.NewEncryptingStorer(storePath, secure)
		logger.Info("凭据静态存储加密已启用", "backend", backend)
	}
	ring := accesskey.NewRing()
	if keys, err := store.Load(); err != nil {
		return nil, nil, fmt.Errorf("载入凭据 store 失败（fail-closed）: %w", err)
	} else if len(keys) > 0 {
		if rerr := ring.Replace(keys); rerr != nil {
			return nil, nil, fmt.Errorf("重建凭据 Ring 失败: %w", rerr)
		}
		logger.Info("已从凭据 store 载入", "keys", len(keys))
	}
	// U3：不生成 anonymous——空 store = 零凭据等待注册。
	if ring.Len() == 0 {
		logger.Info("零凭据启动：请在本机回环执行 /api/credentials/register，首个注册者将成为 admin")
	}
	return ring, store, nil
}

// resolveCredentialMasterKey 解析 credential_store.encrypt=true 装配所需的 32B master
// key（单一事实源 = accesskey 的 LoadMasterKeyFromFile / MasterKeyFromBase64，本层只做
// 读取与来源选择，不写 AES/HKDF）。来源顺序：master_key_file 文件 > 环境变量
// CredentialMasterKeyEnv；两者都无 → error（fail-fast，防启动后解密失败用空凭据表运行）。
func resolveCredentialMasterKey(cfg *Config) ([]byte, error) {
	if cfg.CredentialStore.MasterKeyFile != "" {
		key, err := accesskey.LoadMasterKeyFromFile(cfg.CredentialStore.MasterKeyFile)
		if err != nil {
			return nil, fmt.Errorf("读取 credential_store.master_key_file 失败: %w", err)
		}
		return key, nil
	}
	if v := os.Getenv(CredentialMasterKeyEnv); v != "" {
		key, err := accesskey.MasterKeyFromBase64(v)
		if err != nil {
			return nil, fmt.Errorf("解析环境变量 %s 失败: %w", CredentialMasterKeyEnv, err)
		}
		return key, nil
	}
	return nil, fmt.Errorf("credential_store.encrypt=true 需配置 credential_store.master_key_file 或环境变量 %s（base64 编码 32B master key）", CredentialMasterKeyEnv)
}

// resolveVaultToken 解析 backend=vault 装配所需的 Vault token（来源顺序：token_file 文件
// （读入后 TrimSpace）> TokenEnv 环境变量 > error fail-fast）。token 值只用于构造
// VaultTransitStorer（HTTP 头），不落日志。
func resolveVaultToken(vc VaultConfig) (string, error) {
	if vc.TokenFile != "" {
		data, err := os.ReadFile(vc.TokenFile)
		if err != nil {
			return "", fmt.Errorf("读取 vault token 文件失败: %w", err)
		}
		tok := strings.TrimSpace(string(data))
		tok = strings.TrimPrefix(tok, "\uFEFF") // 清 UTF-8 BOM（Windows 编辑的 token 文件常带，否则 403 难排查）
		if tok != "" {
			return tok, nil
		}
	}
	envName := vc.TokenEnv
	if envName == "" {
		envName = "VAULT_TOKEN"
	}
	if tok := os.Getenv(envName); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("credential_store.backend=vault 需配置 token_file 或环境变量 %s", envName)
}

// bestFirstCredential 返回 Ring 中首个可用（alive）AK 及其 64-hex SK。
// 供 xfer listener 装配（取代 cfg.AccessKeys[0]）使用。ring 为空 / 无可存活着
// 返回 ("", "", false)。
func bestFirstCredential(ring *accesskey.Ring) (ak, skHexStr string, ok bool) {
	ak, skHexStr, _, ok = bestCredential(ring)
	return ak, skHexStr, ok
}

// bestCredential 返回 Ring 中首个存活条目的 (AK, 64-hex SK, skeyID)。
//
// skeyID 是 v2 签名协议必传的 `skey-id=` 段（SKEntry.ID，形如 skey-<12hex>）；xfer listener
// 只消费 (AK, SK)，A 侧自用客户端（SelfCredential）三者都要。
func bestCredential(ring *accesskey.Ring) (ak, skHexStr, skeyID string, ok bool) {
	if ring == nil {
		return "", "", "", false
	}
	for _, k := range ring.Snapshot() {
		if e := ring.CoreEntry(k.AK); e != nil {
			return k.AK, skHex(e.SK), e.ID, true
		}
	}
	return "", "", "", false
}

// SelfCredential 返回本服务端**自用**凭据（AK / SK hex / skeyID），供**同进程内**的组件以
// SproxySig 访问本机 HTTP 面（Y 二期 P3-d：A 侧 mesh 中继经本机 hub API
// `/api/hub/services`、`/api/relay/stream`）。
//
// ok=false 表示凭据 Ring 为空或无存活条目 ⇒ 调用方必须 fail-closed（拿一对空串去签名只会
// 得到 401，且掩盖「本机无可用凭据」这一装配事实）。
//
// 安全边界：本方法只暴露「本进程自己的」凭据，不引入任何新的导出面给外部调用者；调用方
// 必须处于同进程（同进程即已持有 Ring 内存副本，故无提权）。
func (h *Handlers) SelfCredential() (ak, skHex, skeyID string, ok bool) {
	return bestCredential(h.credentialRing)
}
