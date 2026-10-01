// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DirSecretStore 是 SecretStore 的目录落盘实现：每个 secret 一个文件
// （0600，仅 owner 可读写），文件名 = secret 名。
//
// **安全说明**：此为**明文**落盘（非加密）。refresh_token 是永久账号接管
// 凭据——目录须视为明文机密度（勿共享/版本库/日志）。加密卷（shardseal）
// 后续片接入时可替换本实现（同样实现 SecretStore 接口即插即用）。
type DirSecretStore struct {
	dir string
}

// NewDirSecretStore 构造目录 secret 存储（目录不存在则创建，仅 owner 可写）。
func NewDirSecretStore(dir string) (*DirSecretStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pikpak secret store: mkdir %s: %w", dir, err)
	}
	return &DirSecretStore{dir: dir}, nil
}

// Dir 返回存储目录。
func (s *DirSecretStore) Dir() string { return s.dir }

// Read 读取 secret 内容。
func (s *DirSecretStore) Read(_ context.Context, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return nil, fmt.Errorf("pikpak secret store: read %s: %w", name, err)
	}
	return b, nil
}

// Write 写入 secret 内容（原子写：临时文件 + rename，避免半写文件被读到）。
func (s *DirSecretStore) Write(_ context.Context, name string, data []byte) error {
	path := filepath.Join(s.dir, name)
	tmp, err := os.CreateTemp(s.dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("pikpak secret store: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败清理
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("pikpak secret store: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pikpak secret store: close %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("pikpak secret store: chmod %s: %w", name, err)
	}
	// Windows 上 os.CreateTemp 默认 0600（与 Unix 一致），Chmod 在 Unix 生效；
	// 这里再对最终目标文件显式收紧权限（覆盖 rename 前临时文件权限被放宽的极端情况）。
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("pikpak secret store: rename %s: %w", name, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("pikpak secret store: chmod final %s: %w", name, err)
	}
	return nil
}

// Delete 删除 secret 文件。
func (s *DirSecretStore) Delete(_ context.Context, name string) error {
	err := os.Remove(filepath.Join(s.dir, name))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pikpak secret store: delete %s: %w", name, err)
	}
	return nil
}

// List 列出所有 secret 文件名（按字母序）。
func (s *DirSecretStore) List(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("pikpak secret store: list %s: %w", s.dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}
