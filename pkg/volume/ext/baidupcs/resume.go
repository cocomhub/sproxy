// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// resumePath 返回 key 对应的断点文件路径（<Resume>/<sanitized>.json）。
func (l *Layout) resumePath(key string) string {
	return filepath.Join(l.Resume, l.SanitizeKey(key)+".json")
}

// SaveResume 原子写断点状态：先写 tmp 再 rename（仿 pkg/store 原子写模式），
// 避免崩溃/中断时留下半截 JSON。
func (l *Layout) SaveResume(key string, state any) error {
	if state == nil {
		return fmt.Errorf("%w: resume state is nil", ErrInvalidParam)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("baidupcs: marshal resume %q: %w", key, err)
	}
	path := l.resumePath(key)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("baidupcs: write resume tmp %q: %w", key, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("baidupcs: rename resume %q: %w", key, err)
	}
	return nil
}

// LoadResume 读取断点状态到 out（json.Unmarshal 语义）。
// 文件不存在或损坏均返回错误（调用方据此判定「无断点可恢复」）。
func (l *Layout) LoadResume(key string, out any) error {
	if out == nil {
		return fmt.Errorf("%w: resume out is nil", ErrInvalidParam)
	}
	path := l.resumePath(key)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("baidupcs: resume %q not found: %w", key, os.ErrNotExist)
		}
		return fmt.Errorf("baidupcs: read resume %q: %w", key, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("baidupcs: unmarshal resume %q: %w", key, err)
	}
	return nil
}

// DeleteResume 删除断点文件（不存在时视为成功——上传完成后清理用）。
func (l *Layout) DeleteResume(key string) error {
	path := l.resumePath(key)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("baidupcs: remove resume %q: %w", key, err)
	}
	return nil
}
