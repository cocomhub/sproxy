// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LayoutVersion 是当前存储布局版本。OpenRoot 时校验/写入 <root>/LAYOUT_VERSION。
const LayoutVersion = "2"

// layoutVersionFile 是布局版本标记文件名。
const layoutVersionFile = "LAYOUT_VERSION"

// Root 封装 os.Root，附带布局版本校验。所有文件操作相对 root，防穿越/符号链接逃逸由标准库保证。
// os.Root 已覆盖 MkdirAll/Chtimes（Go 1.26），本封装直接委托；Abs 派生绝对路径供 os.SameFile 等。
type Root struct {
	r    *os.Root
	base string // 绝对路径（供 Abs/Chtimes 等派生）
}

// OpenRoot 打开 storage root 目录并校验/写入 LAYOUT_VERSION。
// 版本文件不存在则写入当前 LayoutVersion；存在但内容不匹配则返回错误（迁移钩子）。
// path 必须已存在（由装配层创建），未创建时 os.OpenRoot 报错。
func OpenRoot(path string) (*Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("storage: OpenRoot(%s): %w", path, err)
	}
	base, err := filepath.Abs(path)
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("storage: Abs(%s): %w", path, err)
	}
	rt := &Root{r: r, base: base}
	if err := rt.ensureLayoutVersion(); err != nil {
		r.Close()
		return nil, err
	}
	return rt, nil
}

// ensureLayoutVersion 读取/写入布局版本标记：不存在则写入，不匹配则报错。
// 走 os.Root.ReadFile/WriteFile 保持 root 相对且符号链接不逃逸。
func (rt *Root) ensureLayoutVersion() error {
	data, err := rt.r.ReadFile(layoutVersionFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rt.r.WriteFile(layoutVersionFile, []byte(LayoutVersion+"\n"), 0o644)
		}
		return fmt.Errorf("storage: 读取 LAYOUT_VERSION: %w", err)
	}
	if got := strings.TrimSpace(string(data)); got != LayoutVersion {
		return fmt.Errorf("storage: LAYOUT_VERSION 不匹配: 当前 %q 期望 %q", got, LayoutVersion)
	}
	return nil
}

// Open 相对 root 打开文件。
func (rt *Root) Open(rel string) (*os.File, error) {
	return rt.r.Open(rel)
}

// OpenFile 相对 root 打开/创建文件。
func (rt *Root) OpenFile(rel string, flag int, perm os.FileMode) (*os.File, error) {
	return rt.r.OpenFile(rel, flag, perm)
}

// Stat 返回相对路径的文件信息。
func (rt *Root) Stat(rel string) (os.FileInfo, error) {
	return rt.r.Stat(rel)
}

// MkdirAll 相对 root 递归创建目录；已存在时幂等。
func (rt *Root) MkdirAll(rel string, perm os.FileMode) error {
	return rt.r.MkdirAll(rel, perm)
}

// Remove 相对 root 删除单个文件/空目录。
func (rt *Root) Remove(rel string) error {
	return rt.r.Remove(rel)
}

// RemoveAll 相对 root 递归删除。
func (rt *Root) RemoveAll(rel string) error {
	return rt.r.RemoveAll(rel)
}

// Rename 相对 root 重命名/移动。
func (rt *Root) Rename(oldRel, newRel string) error {
	return rt.r.Rename(oldRel, newRel)
}

// Chtimes 相对 root 修改访问/修改时间。委托 os.Root.Chtimes，随标准库保证 root 内约束。
func (rt *Root) Chtimes(rel string, atime, mtime time.Time) error {
	return rt.r.Chtimes(rel, atime, mtime)
}

// Abs 派生 root 内 rel 对应的绝对路径并确认仍在 base 内（字符串级校验，供 os.SameFile 等）。
// rel 必须相对（拒绝绝对路径与 .. 段，反斜杠视为分隔符）；非法返回 ("", false)。
func (rt *Root) Abs(rel string) (string, bool) {
	if rel == "" {
		return rt.base, true
	}
	norm := strings.ReplaceAll(rel, `\`, "/")
	if strings.HasPrefix(norm, "/") || filepath.IsAbs(norm) {
		return "", false
	}
	if v := filepath.VolumeName(norm); v != "" {
		return "", false
	}
	clean := filepath.Clean(norm)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	abs := filepath.Join(rt.base, clean)
	if abs != rt.base && !strings.HasPrefix(abs, rt.base+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

// Close 关闭 root 句柄。
func (rt *Root) Close() error {
	return rt.r.Close()
}
