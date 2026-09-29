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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/state"
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

// credentialRingKey 是凭据 Ring 在 StateStore 中的 key（statestore.md §2.3 三段式
// <owner>/<type>/<name>；credential 迁移矩阵 P0，见 §5.1）。
const credentialRingKey = "credential/anonymous/ring"

// stateBackedCredentialStore 是凭据 Ring 的 StateStore 适配（statestore.md §5.1 P0）：
// 实现 accesskey.CredentialStorer，Load/Save 委托 StateStore.Get/Put（key 见
// credentialRingKey），磁盘字节与既有 credentialsFile（{version, keys}）JSON 逐字一致。
//
// 双读单写（零回归铁律，statestore.md §5.1）：
//   - Load：StateStore.Get 优先；ErrKeyNotFound → 回退读旧 <meta>/credentials.json
//     （legacyPath，迁移前存量零丢失）；旧文件也不存在 → (nil, nil)（U3 零凭据启动）；
//   - Save：恒写 StateStore 新路径（首写即完成迁移；旧 meta 文件不再改写）。
//
// secure 非 nil（credential_store.encrypt=true 时由装配层注入）：StateStore 值为密文字节，
// Vault/aesgcm 语义与 EncryptingStorer 一致（StateStore 之上做字节级加解密）。
type stateBackedCredentialStore struct {
	st         state.StateStore
	key        string
	legacyPath string // 旧 <meta>/credentials.json（回退读）；空 = 不回退
	secure     accesskey.SecureStorer
}

var _ accesskey.CredentialStorer = (*stateBackedCredentialStore)(nil)

// newStateBackedCredentialStore 构造 StateStore 后端凭据 store。
func newStateBackedCredentialStore(st state.StateStore, key, legacyPath string, secure accesskey.SecureStorer) *stateBackedCredentialStore {
	return &stateBackedCredentialStore{st: st, key: key, legacyPath: legacyPath, secure: secure}
}

// stateBackedCredentialsFile 是凭据快照的磁盘格式（与 accesskey.CredentialStore 的
// credentialsFile 结构同形：version=1 + keys）。accesskey 包的该结构未导出，故在
// 装配层声明等价结构（与 accesskey/encrypting_storer.go 的 encryptedCredentialsFile
// 同款裁定）。
type stateBackedCredentialsFile struct {
	Version int             `json:"version"`
	Keys    []accesskey.Key `json:"keys"`
}

// stateLooksLikePlaintextJSON 是明文凭据 JSON 嗅探（与 accesskey 包内
// looksLikePlaintextJSON 同语义：首字节 '{' 且含 "keys"）——供加密态 Load 解密失败时
// 定向诊断「文件仍为明文未迁移」。仅影响错误文案，不影响 fail-closed 语义。
func stateLooksLikePlaintextJSON(data []byte) bool {
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	return bytes.Contains(data, []byte(`"keys"`))
}

// Load 读凭据快照（StateStore 优先；未命中回退旧 meta 文件）。
// 值损坏（JSON 解析失败 / 结构非法）返回 error（fail-closed，凭据是权威，不静默重建）。
func (s *stateBackedCredentialStore) Load() ([]accesskey.Key, error) {
	data, found, err := s.readCredentialsData()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil // U3：零凭据启动
	}
	if s.secure != nil {
		data, err = s.decryptCredentials(data)
		if err != nil {
			return nil, err
		}
	}
	var f stateBackedCredentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("credentials store: 解析失败（文件损坏，拒绝覆盖）: %w", err)
	}
	return f.Keys, nil
}

// readCredentialsData 读取凭据快照原始字节：StateStore.Get 优先；ErrKeyNotFound 回退
// 读旧 <meta>/credentials.json（迁移前存量）。found=false 表示确实无凭据数据
// （StateStore 未命中且无 legacy 可读）——调用方据此返回 (nil, nil)（U3 零凭据启动），
// 不再进入解析。
func (s *stateBackedCredentialStore) readCredentialsData() ([]byte, bool, error) {
	ctx := context.Background()
	data, err := s.st.Get(ctx, s.key)
	if err != nil {
		if !errors.Is(err, state.ErrKeyNotFound) {
			return nil, false, fmt.Errorf("credentials store: StateStore 读取失败: %w", err)
		}
		// 回退读旧 meta（迁移前存量）。
		if s.legacyPath != "" {
			data, err = os.ReadFile(s.legacyPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, false, nil // U3：零凭据启动
				}
				return nil, false, fmt.Errorf("credentials store: 读取旧 %s 失败: %w", s.legacyPath, err)
			}
			return data, true, nil
		}
		return nil, false, nil
	}
	return data, true, nil
}

// decryptCredentials 解密凭据快照字节（secure 已装配时）。解密失败按数据形态给定向
// 诊断：明文 JSON（未迁移）vs 密文被篡改 / master key 不匹配——两者都 fail-closed。
func (s *stateBackedCredentialStore) decryptCredentials(data []byte) ([]byte, error) {
	pt, derr := s.secure.Decrypt(data)
	if derr != nil {
		if stateLooksLikePlaintextJSON(data) {
			return nil, fmt.Errorf("credentials store: 解密失败——文件仍为明文 JSON 未迁移（credential_store.encrypt=true 开启前既有凭据文件需先迁移为密文；fail-closed 拒绝，不静默重建）: %w", derr)
		}
		return nil, fmt.Errorf("credentials store: 解密失败——密文被篡改 / master key 不匹配（fail-closed 拒绝，不静默重建）: %w", derr)
	}
	return pt, nil
}

// Save 全量快照写 StateStore（序列化格式与 credentialstore.go 一致：{version, keys}）。
// secure 非 nil 时先加密再写（StateStore 值为密文）。
func (s *stateBackedCredentialStore) Save(keys []accesskey.Key) error {
	ctx := context.Background()
	f := stateBackedCredentialsFile{Version: 1, Keys: keys}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials store: 序列化失败: %w", err)
	}
	if s.secure != nil {
		data, err = s.secure.Encrypt(data)
		if err != nil {
			return fmt.Errorf("credentials store: 加密失败: %w", err)
		}
	}
	if err := s.st.Put(ctx, s.key, data); err != nil {
		return fmt.Errorf("credentials store: StateStore 写入失败: %w", err)
	}
	return nil
}

// BootstrapServerCredentials 是生产装配入口：为服务端准备凭据 Ring + store
// （供 cmd/sproxy 在 RegisterRoutes 与 hub 装配之前调用，随后把二者注入 opts）。
//   - 单节点默认（无 state_store 段 / 显式 local + cluster 关）：store = <默认卷根>
//     /anonymous/meta/credentials.json（服务端级全局凭据，anonymous 租户的 meta 桶；
//     多租户部署如需 per-owner 凭据经 /api/credentials 管理，见任务 5）。
//     **默认卷根经 resolveDefaultVolumeRoot 裁决**（非 cfg.StorageRoot）——显式
//     volumes[0].root ≠ storage_root 分叉时凭据必须落默认卷 meta（AD-5 meta 归属默认卷
//     不变式；否则重启凭据 Ring 丢失，PR-B 终审建议 9）。
//   - 集群模式（cluster.enabled=true 或 state_store.type != local）→ store 切 StateStore
//     后端（stateBackedCredentialStore，StateStore.Get/Put，key=credential/anonymous/ring），
//     读旧 meta 回退 + 首写迁新路径（双读单写零回归）；StateStore 装配失败 → 启动失败
//     （fail-closed，防「以为多节点一致、实际各写各的」）。
//   - 载入既有快照；**U3：不再生成首启 anonymous 凭据**——store 为空则返回空 Ring，
//     系统以零凭据等待 register 公开端点（首个回环注册者授 admin）。
func BootstrapServerCredentials(cfg *Config, logger *slog.Logger) (*accesskey.Ring, accesskey.CredentialStorer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	metaDir := filepath.Join(resolveDefaultVolumeRoot(cfg), anonymousOwner, "meta")
	legacyPath := filepath.Join(metaDir, fileNameCredStore)
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

// newStateBackedStore 装配 StateStore 后端（集群模式凭据必选 StateStore）。失败（type/dir
// 配置非法或后端不可达）返回错误——fail-closed，防「以为多节点一致、实际各写各的」。
func newStateBackedStore(cfg *Config, logger *slog.Logger) (state.StateStore, error) {
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
func buildEncryptedCredentialStore(cfg *Config, stateStore bool, st state.StateStore, metaDir, legacyPath string, logger *slog.Logger) (accesskey.CredentialStorer, error) {
	storePath := filepath.Join(metaDir, fileNameCredStore)
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
			AADPath: filepath.ToSlash(filepath.Join(anonymousOwner, "meta", fileNameCredStore)),
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
		masterKey, err := resolveCredentialMasterKey(cfg)
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

// resolveCredentialMasterKey 解析 credential_store.encrypt=true 装配所需的 32B master
// key（单一事实源 = accesskey 的 LoadMasterKeyFromFile / MasterKeyFromBase64，本层只做
// 读取与来源选择，不写 AES/HKDF）。来源顺序：master_key_file 文件 > 环境变量
// CredentialMasterKeyEnv；两者都无 → error（fail-fast，防启动后解密失败用空凭据表运行）。
// 导出（ResolveCredentialMasterKey）：装配层复用同一 master key 加密用户卷 Extra（审查 P2）。
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

// ResolveCredentialMasterKey 导出 resolveCredentialMasterKey（供 cmd/sproxy 装配层复用，
// 加密用户卷 Extra——审查 P2）。语义与内部实现一致。
func ResolveCredentialMasterKey(cfg *Config) ([]byte, error) {
	return resolveCredentialMasterKey(cfg)
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
