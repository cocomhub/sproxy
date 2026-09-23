// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// user_volume_store.go 实现用户自有卷（per-owner volume）的持久化：每 owner 的
// volume 元数据存 `<storage_root>/<owner>/meta/volume/<name>.json`（仿凭据 store
// 布局与原子写）。重启后由 ScanRestore 扫描恢复（装配层并入 registry.Set）。
//
// 用户卷（用户 2026-09-17 确认）：仅外部类型（baidupcs 等 backend 插件注册的）、
// 独立卷容量（vol_capacity 不计 owner 配额）、owner 专属（跨用户 404 防枚举）。
//
// 与凭据 store（pkg/accesskey.CredentialStore）同构：临时文件 + fsync + rename 原子写，
// per-owner 锁串行化防 Windows 并发 rename 覆盖。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// UserVolume 是用户自有卷的持久化描述（JSON 友好）。
type UserVolume struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // 卷后端类型（仅外部类型：baidupcs 等已注册 backend）
	Owner    string `json:"owner"`
	Capacity int64  `json:"capacity"` // 独立卷容量（0 = 不限制）
	// Usage 是本系统当前已占用该卷的字节（C3 查询 API 填充；0 = 无计数/未装配）。
	Usage int64          `json:"usage,omitempty"`
	Extra map[string]any `json:"extra,omitempty"` // 类型特有配置（bduss/baidu_root/binary_path/local_root）
	// ExtraEnc 是 Extra 的加密信封（base64：nonce || AES-256-GCM 密文）。
	// masterKey 启用时落盘（明文 Extra 不落盘）；旧文件/未启用时为空（读明文 Extra）。
	ExtraEnc string `json:"extra_enc,omitempty"`
}

// UserVolumeStore 把每 owner 的卷元数据持久化到
// `<root>/<owner>/meta/volume/<name>.json`。
//
// 敏感字段保护（审查 P2，2026-09-23）：Extra 可能含后端凭据（bduss/access_key_secret 等），
// 此前 JSON 明文落盘（与凭据 store 的 AESGCM 加密不一致）。本修复：
//   - masterKey 非 nil 时，Create 前把 Extra 加密为 `extra_enc`（base64 信封：
//     nonce || AES-256-GCM 密文），落盘不含明文 Extra；Get/List 解密回填 Extra。
//   - masterKey 为 nil（旧装配/未启用 credential_store.encrypt）时保持明文兼容
//     （旧文件可读；新写仍明文——运维应配 master_key_file 后重启启用加密）。
//   - 权限：目录 0755、文件 0600（此前 0644）——未启用加密时也降低暴露面。
type UserVolumeStore struct {
	root      string
	masterKey []byte     // 32B AES-256 master key（nil = 不加密，旧行为）
	muMu      sync.Mutex // 守卫 ownerLocks map
	// ownerLocks 是 owner → 该 owner 的写锁（每 owner 独立串行化原子写，
	// 防 Windows 并发 rename 到同文件 Access denied；跨 owner 不互斥）。
	ownerLocks map[string]*sync.Mutex
}

// NewUserVolumeStore 创建绑定到存储根（storage_root）的 store。owner 目录在
// <root>/<owner>/meta/volume/ 下（meta 桶仿凭据布局）。masterKey 可选：非 nil 时
// Extra 加密落盘（对齐凭据 store），nil 时明文（旧行为/未启用加密）。
func NewUserVolumeStore(root string, masterKey ...[]byte) *UserVolumeStore {
	s := &UserVolumeStore{root: root, ownerLocks: map[string]*sync.Mutex{}}
	if len(masterKey) > 0 && len(masterKey[0]) == 32 {
		s.masterKey = masterKey[0]
	}
	return s
}

// lockFor 返回 owner 的写锁（懒创建）。
func (s *UserVolumeStore) lockFor(owner string) *sync.Mutex {
	s.muMu.Lock()
	defer s.muMu.Unlock()
	l, ok := s.ownerLocks[owner]
	if !ok {
		l = &sync.Mutex{}
		s.ownerLocks[owner] = l
	}
	return l
}

// dirFor 返回 owner 的卷目录（<root>/<owner>/meta/volume/）。
func (s *UserVolumeStore) dirFor(owner string) string {
	return filepath.Join(s.root, owner, "meta", "volume")
}

// Root 返回用户卷 store 的存储根（counter 持久化用：<root>/<owner>/meta/volume/<name>.capacity.json）。
func (s *UserVolumeStore) Root() string { return s.root }

// pathFor 返回 owner 的卷文件路径。
func (s *UserVolumeStore) pathFor(owner, name string) string {
	return filepath.Join(s.dirFor(owner), name+".json")
}

// validate 校验卷描述（创建时）：owner/name 非空且为合法段名（无路径穿越），
// type 非空（外部类型校验在 API 层）。
func (s *UserVolumeStore) validate(owner string, v UserVolume) error {
	if !storage.ValidSegmentName(owner) {
		return fmt.Errorf("用户卷 store: 非法 owner %q", owner)
	}
	if !storage.ValidSegmentName(v.Name) {
		return fmt.Errorf("用户卷 store: 非法卷名 %q（拒绝路径穿越/非法字符）", v.Name)
	}
	if strings.TrimSpace(v.Type) == "" {
		return fmt.Errorf("用户卷 store: 卷 %q type 为空（仅外部类型：baidupcs 等）", v.Name)
	}
	return nil
}

// Create 持久化新卷（原子写）。重名 → 明确错误；owner 目录自动创建。
// masterKey 非 nil 时 Extra 加密落盘（extra_enc），明文 Extra 不落盘。
func (s *UserVolumeStore) Create(owner string, v UserVolume) error {
	if err := s.validate(owner, v); err != nil {
		return err
	}
	v.Owner = owner // owner 以参数为准（防描述字段篡改）
	lock := s.lockFor(owner)
	lock.Lock()
	defer lock.Unlock()
	if err := os.MkdirAll(s.dirFor(owner), 0o755); err != nil {
		return fmt.Errorf("用户卷 store: 创建目录失败: %w", err)
	}
	path := s.pathFor(owner, v.Name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("用户卷 store: 卷 %q（owner %q）已存在", v.Name, owner)
	}
	// 敏感 Extra 加密（审查 P2）：masterKey 非 nil 时把 Extra JSON 封进 extra_enc，
	// 落盘不含明文 Extra（对齐凭据 store EncryptWithKey）。nil 时明文兼容（旧装配）。
	if s.masterKey != nil && len(v.Extra) > 0 {
		if enc, err := s.encryptExtra(v.Extra); err != nil {
			return fmt.Errorf("用户卷 store: 加密 Extra 失败: %w", err)
		} else {
			v.ExtraEnc = enc
			v.Extra = nil
		}
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("用户卷 store: 序列化失败: %w", err)
	}
	return s.writeFileAtomic(path, data)
}

// Get 读取指定卷；不存在返回 (nil, nil)。
// masterKey 非 nil 且落盘含 extra_enc 时解密回填 Extra；无 extra_enc（明文旧文件）
// 直接读 Extra（旧装配兼容）；解密失败 fail-closed（返回错误，不静默返回明文）。
func (s *UserVolumeStore) Get(owner, name string) (*UserVolume, error) {
	if !storage.ValidSegmentName(owner) || !storage.ValidSegmentName(name) {
		return nil, nil
	}
	data, err := os.ReadFile(s.pathFor(owner, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("用户卷 store: 读取 %s 失败: %w", s.pathFor(owner, name), err)
	}
	var v UserVolume
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("用户卷 store: 解析 %s 失败（文件损坏，拒绝静默覆盖）: %w", s.pathFor(owner, name), err)
	}
	if v.ExtraEnc != "" {
		if s.masterKey == nil {
			return nil, fmt.Errorf("用户卷 store: 卷 %q 含加密 Extra 但未配置 master_key（无法解密）", name)
		}
		extra, err := s.decryptExtra(v.ExtraEnc)
		if err != nil {
			return nil, fmt.Errorf("用户卷 store: 解密卷 %q Extra 失败: %w", name, err)
		}
		v.Extra = extra
	}
	return &v, nil
}

// encryptExtra 把 Extra（map）JSON 序列化后用 masterKey 加密为 base64 信封。
func (s *UserVolumeStore) encryptExtra(extra map[string]any) (string, error) {
	data, err := json.Marshal(extra)
	if err != nil {
		return "", err
	}
	enc, err := accesskey.EncryptWithKey(s.masterKey, data)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// decryptExtra 解密 extra_enc（base64 信封 → JSON → map）。
func (s *UserVolumeStore) decryptExtra(enc string) (map[string]any, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("base64 解码失败: %w", err)
	}
	plain, err := accesskey.DecryptWithKey(s.masterKey, raw)
	if err != nil {
		return nil, err
	}
	var extra map[string]any
	if err := json.Unmarshal(plain, &extra); err != nil {
		return nil, err
	}
	return extra, nil
}

// ListByOwner 返回 owner 的全部卷（按名排序）。
func (s *UserVolumeStore) ListByOwner(owner string) ([]UserVolume, error) {
	if !storage.ValidSegmentName(owner) {
		return nil, nil
	}
	dir := s.dirFor(owner)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("用户卷 store: 扫描 %s 失败: %w", dir, err)
	}
	out := make([]UserVolume, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		v, err := s.Get(owner, name)
		if err != nil {
			return nil, err
		}
		if v != nil {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete 删除指定卷；不存在 → 明确错误（fail-closed：静默 no-op 会掩盖调用方卷名笔误）。
func (s *UserVolumeStore) Delete(owner, name string) error {
	if !storage.ValidSegmentName(owner) || !storage.ValidSegmentName(name) {
		return fmt.Errorf("用户卷 store: 非法 owner/卷名 %q/%q", owner, name)
	}
	lock := s.lockFor(owner)
	lock.Lock()
	defer lock.Unlock()
	path := s.pathFor(owner, name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("用户卷 store: 卷 %q（owner %q）不存在", name, owner)
		}
		return fmt.Errorf("用户卷 store: 检查 %s 失败: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("用户卷 store: 删除 %s 失败: %w", path, err)
	}
	return nil
}

// ScanRestore 扫描全部 owner 的 meta/volume/，返回全部用户卷（重启装配恢复用）。
// 扫描过滤内部目录（.__ / __ 前缀，仿 storage.ListOwners）。
func (s *UserVolumeStore) ScanRestore() ([]UserVolume, error) {
	baseEntries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("用户卷 store: 扫描根 %s 失败: %w", s.root, err)
	}
	var out []UserVolume
	for _, e := range baseEntries {
		if !e.IsDir() {
			continue
		}
		owner := e.Name()
		if !storage.ValidSegmentName(owner) {
			continue
		}
		if strings.HasPrefix(owner, ".__") || strings.HasPrefix(owner, "__") {
			continue // 内部目录，非租户根
		}
		vols, err := s.ListByOwner(owner)
		if err != nil {
			return nil, err
		}
		out = append(out, vols...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// writeFileAtomic 用「临时文件 + fsync + rename」原子写（防 Windows 并发 rename 覆盖）。
// 调用方须已持有 owner 锁。文件权限 0600（审查 P2：Extra 可能含凭据，收紧默认权限）。
func (s *UserVolumeStore) writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "*.json.tmp")
	if err != nil {
		return fmt.Errorf("用户卷 store: 创建临时文件失败: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("用户卷 store: 设置临时文件权限失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 no-op；失败时清理残留
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("用户卷 store: 写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("用户卷 store: fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("用户卷 store: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("用户卷 store: 原子重命名失败: %w", err)
	}
	return nil
}
