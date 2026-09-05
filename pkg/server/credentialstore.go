// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// CredentialStore 把 Ring 快照持久化到 `<tenant>/meta/credentials.json`，
// 作为进程重启后凭据恢复的单一权威（凭据 store 化）。
type CredentialStore struct {
	path   string
	saveMu sync.Mutex // 串行化 Save：防止 Windows 上并发 Rename 覆盖既有文件失败
}

// NewCredentialStore 创建绑定到 metaDir 的 store。metaDir 由调用方按租户计算
// （如 `<storage_root>/<owner>/meta`）。
func NewCredentialStore(metaDir string) *CredentialStore {
	return &CredentialStore{path: filepath.Join(metaDir, "credentials.json")}
}

// credentialsFile 是 credentials.json 的磁盘格式（Key 列表，SK 以 []byte → base64）。
type credentialsFile struct {
	Version int             `json:"version"`
	Keys    []accesskey.Key `json:"keys"`
}

// Load 读取 store 中的 Ring 快照。文件不存在返回 (nil, nil)（首次启动）。
// 文件存在但内容非法（JSON 解析失败 / 结构损坏）返回错误（fail-closed，不静默重建）。
func (s *CredentialStore) Load() ([]accesskey.Key, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("credentials store: 读取 %s 失败: %w", s.path, err)
	}
	var f credentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("credentials store: 解析 %s 失败（文件损坏，拒绝覆盖）: %w", s.path, err)
	}
	return f.Keys, nil
}

// Save 用「临时文件 + rename」原子写把 Ring 快照落盘。saveMu 串行化保证并发
// Save 不损坏（Windows 上并发 rename 到同一目标会 Access denied）。目录不存在
// 时自动 MkdirAll。
func (s *CredentialStore) Save(keys []accesskey.Key) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("credentials store: 创建目录失败: %w", err)
	}
	f := credentialsFile{Version: 1, Keys: keys}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials store: 序列化失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "credentials.json.tmp*")
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

// newAnonymousKey 生成首启 anonymous 凭据的 AK/SK。
// 机理与 pkg/accesskey.GeneratePair（"") 同源（服务端不 import cmd/sclient 跨模块，
// 此处内联等价实现）：AK = sk-<16B hex>，SK = 32B 随机 hex。
func newAnonymousKey() (ak, sk string, err error) {
	akBytes := make([]byte, 16)
	if _, err := rand.Read(akBytes); err != nil {
		return "", "", fmt.Errorf("credentials store: 生成 anonymous AK 失败: %w", err)
	}
	skBytes := make([]byte, 32)
	if _, err := rand.Read(skBytes); err != nil {
		return "", "", fmt.Errorf("credentials store: 生成 anonymous SK 失败: %w", err)
	}
	return "sk-" + hex.EncodeToString(akBytes), hex.EncodeToString(skBytes), nil
}
