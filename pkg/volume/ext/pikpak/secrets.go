// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// validSecretStoreName 校验 secret 存储键名：非空、不含路径分隔符 `/` `\`、不为 `.`/`..`
// （路径穿越防御：name 会直接作为底层 FS 的文件路径使用，须与 secrets 卷 validSecretName
// 同规则；账号名已在 Add 校验，此处防御性再查，防外部 SecretStore 实现/手动文件命中）。
func validSecretStoreName(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return false
	}
	return name != "." && name != ".."
}

// FSSecretStore 是 SecretStore 的任意 sync.FS 落盘实现：每个 secret 一个文件，文件名 =
// secret 名。**加密与否取决于底层 FS**——装配层注入 secretdata 加密卷（shardseal）时
// 内容加密落盘；注入本地盘时即明文（仅测试/显式明文场景，生产走加密装配）。
// 线程安全：底层 FS 自身保证。
type FSSecretStore struct {
	fs syncpkg.FS
}

// NewFSSecretStore 构造 FS 落盘 secret store（底层 FS 由装配层注入）。
func NewFSSecretStore(fs syncpkg.FS) *FSSecretStore {
	return &FSSecretStore{fs: fs}
}

// FS 返回底层 FS 视图（供装配层/测试检视）。
func (s *FSSecretStore) FS() syncpkg.FS { return s.fs }

// Read 读取 secret 内容。
func (s *FSSecretStore) Read(ctx context.Context, name string) ([]byte, error) {
	if !validSecretStoreName(name) {
		return nil, fmt.Errorf("pikpak secret store: 非法 secret 名 %q", name)
	}
	rc, err := s.fs.OpenRead(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("pikpak secret store: 打开 %s: %w", name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("pikpak secret store: 读 %s: %w", name, err)
	}
	return b, nil
}

// Write 写入 secret 内容（覆盖；mtime 不敏感，传 0）。
func (s *FSSecretStore) Write(ctx context.Context, name string, data []byte) error {
	if !validSecretStoreName(name) {
		return fmt.Errorf("pikpak secret store: 非法 secret 名 %q", name)
	}
	if err := s.fs.WriteFile(ctx, name, bytes.NewReader(data), int64(len(data)), 0); err != nil {
		return fmt.Errorf("pikpak secret store: 写 %s: %w", name, err)
	}
	return nil
}

// Delete 删除 secret（不存在视为成功，幂等）。
func (s *FSSecretStore) Delete(ctx context.Context, name string) error {
	if !validSecretStoreName(name) {
		return fmt.Errorf("pikpak secret store: 非法 secret 名 %q", name)
	}
	err := s.fs.Delete(ctx, name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pikpak secret store: 删 %s: %w", name, err)
	}
	return nil
}

// List 列出所有 secret 文件名（按字母序）。
func (s *FSSecretStore) List(ctx context.Context) ([]string, error) {
	entries, err := s.fs.ListDir(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("pikpak secret store: 列目录: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		names = append(names, e.Name)
	}
	return names, nil
}
