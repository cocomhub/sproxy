// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// encryptedCredentialsFile 是凭据文件明文 JSON 的磁盘格式（EncryptingStorer 加密态下
// Encrypt 的输入 / Decrypt 的输出）。与 server.credentialsFile 结构一致（Keys 类型同为
// []Key，序列化天然同形）——但 accesskey 不能引用 server 的未导出类型，故在包内声明
// 等价结构（4C-2 裁定）。
type encryptedCredentialsFile struct {
	Version int   `json:"version"`
	Keys    []Key `json:"keys"`
}

// EncryptingStorer 是凭据文件的加密静态存储：实现 CredentialStorer（Save 收 []Key），
// 内部把 keys marshal 成 {version, keys} JSON → secure.Encrypt 得密文 → 原子写盘；
// Load 读盘 → secure.Decrypt 还原 JSON → unmarshal keys。secure 为 nil 时明文直读直写
// （等价 PlainStorer 语义，但保留 EncryptingStorer 结构）。path 由构造注入。
//
// 磁盘格式：未加密 = 明文 JSON（与 server.CredentialStore 字节一致）；加密 =
// Encrypt 输出密文字节（无外层 JSON 壳，明文 JSON 即 Encrypt 的输入）。
type EncryptingStorer struct {
	path   string
	secure SecureStorer // nil = 明文（未开启加密）
	saveMu sync.Mutex   // 串行化 Save（Windows 并发 Rename 需退避——仿 server.CredentialStore.saveMu）
}

// 编译期断言：*EncryptingStorer 满足 accesskey.CredentialStorer（与 server 版同款模式）。
var _ CredentialStorer = (*EncryptingStorer)(nil)

// NewEncryptingStorer 创建绑定到 credentials.json 路径的加密 store。secure 传 nil 时
// 为明文模式（读写原始 JSON，磁盘字节与 server.CredentialStore 一致）；传 AESGCMStorer
// 等 SecureStorer 实现时对整份凭据文件做字节级加密。
func NewEncryptingStorer(path string, secure SecureStorer) *EncryptingStorer {
	return &EncryptingStorer{path: path, secure: secure}
}

// Load 读取凭据快照。文件不存在返回 (nil, nil)（首次启动）。加密态下 Decrypt 失败
// （密文篡改 / master key 错 / 磁盘仍是未迁移的明文文件）返回明确 error（fail-closed，
// 不静默重建/改写）。
func (s *EncryptingStorer) Load() ([]Key, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("credentials store: 读取 %s 失败: %w", s.path, err)
	}
	if s.secure != nil {
		pt, err := s.secure.Decrypt(data)
		if err != nil {
			// 解密失败三因（明文未迁移 / 密文篡改 / master key 错）合一难以诊断——先做
			// 廉价嗅探区分明文未迁移（M-3）：明文凭据 JSON 恒以 '{'（0x7b）开头且含
			// "keys" 字段；密文首字节随机，巧合概率 ~1/256 可接受（且还需恰含 "keys"
			// 字节序列，实际可忽略）。真实密文篡改 / 密钥错保持 GCM 失败提示。
			if looksLikePlaintextJSON(data) {
				return nil, fmt.Errorf("credentials store: 解密 %s 失败——文件仍为明文 JSON 未迁移（credential_store.encrypt=true 开启前既有凭据文件需先迁移为密文；fail-closed 拒绝，不静默重建）: %w", s.path, err)
			}
			return nil, fmt.Errorf("credentials store: 解密 %s 失败——密文被篡改 / master key 不匹配（fail-closed 拒绝，不静默重建）: %w", s.path, err)
		}
		data = pt
	}
	var f encryptedCredentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("credentials store: 解析 %s 失败（文件损坏，拒绝覆盖）: %w", s.path, err)
	}
	return f.Keys, nil
}

// Save 用「临时文件 + rename」原子写把凭据快照落盘。saveMu 串行化保证并发 Save 不损坏
// （Windows 上并发 rename 到同一目标会 Access denied）。目录不存在时自动 MkdirAll。
// secure 非 nil 时先加密再写盘（磁盘为密文字节）。
func (s *EncryptingStorer) Save(keys []Key) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("credentials store: 创建目录失败: %w", err)
	}
	f := encryptedCredentialsFile{Version: 1, Keys: keys}
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
	tmp, err := os.CreateTemp(dir, "credentials.json.tmp*")
	if err != nil {
		return fmt.Errorf("credentials store: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 no-op；失败时清理残留
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credentials store: 写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credentials store: fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credentials store: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("credentials store: 原子重命名失败: %w", err)
	}
	return nil
}

// looksLikePlaintextJSON 廉价嗅探磁盘内容是否为明文凭据 JSON（首字节 '{' 且含 "keys"
// 字段字样）——供加密态 Load 解密失败时定向诊断「文件仍为明文未迁移」（M-3）。
// 明文 JSON（MarshalIndent）恒以 0x7b 开头并含 "keys"；密文首字节随机，误判概率
// ~1/256 且还需恰含 "keys" 字节序列，实际可忽略。仅影响错误文案，不影响 fail-closed 语义。
func looksLikePlaintextJSON(data []byte) bool {
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	return bytes.Contains(data, []byte(`"keys"`))
}
