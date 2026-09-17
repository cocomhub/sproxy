// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Layout 是百度网盘插件的本地目录布局。
//
// 用户硬约束：上传/下载等中间状态文件**只依赖本地文件系统**（网盘只存最终文件），
// 故所有 staging/resume/cache/tmp 子目录都落在本地 BaseDir 下：
//
//	<BaseDir>/
//	  staging/  本地上传暂存（配合 quota：预留 → chunk 上传成功释放）
//	  resume/   断点状态（上传 InstanceState / 下载 RangeList 的 JSON）
//	  cache/    下载缓存（可选，LRU 淘汰由上层负责）
//	  tmp/      通用临时文件
type Layout struct {
	// BaseDir 是本地布局根目录（用户配置，如 <本地>/baidupcs）。
	BaseDir string
	// Staging 是本地上传暂存目录。
	Staging string
	// Resume 是断点状态目录。
	Resume string
	// Cache 是下载缓存目录。
	Cache string
	// Tmp 是通用临时文件目录。
	Tmp string
}

// NewLayout 创建布局并确保全部子目录存在。
func NewLayout(base string) (*Layout, error) {
	if strings.TrimSpace(base) == "" {
		return nil, fmt.Errorf("%w: layout base is empty", ErrInvalidParam)
	}
	base = filepath.Clean(base)
	l := &Layout{
		BaseDir: base,
		Staging: filepath.Join(base, "staging"),
		Resume:  filepath.Join(base, "resume"),
		Cache:   filepath.Join(base, "cache"),
		Tmp:     filepath.Join(base, "tmp"),
	}
	for _, dir := range []string{l.BaseDir, l.Staging, l.Resume, l.Cache, l.Tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("baidupcs: mkdir %s: %w", dir, err)
		}
	}
	return l, nil
}

// SanitizeKey 把断点/暂存 key 归一到安全文件名字段：
//   - 目录分隔符（/ \）替换为下划线
//   - 前导点替换为下划线（防隐藏文件/穿越语义）
//   - 其余非法字符替换为下划线
//   - 空输入落 "_"
//
// 归一后的 key 为单一路径段，不可能越出布局根目录。
func (l *Layout) SanitizeKey(key string) string {
	if key == "" {
		return "_"
	}
	out := make([]byte, 0, len(key))
	for i, b := range []byte(key) {
		switch {
		case b == '/' || b == '\\':
			out = append(out, '_')
		case b == '.' && i == 0:
			out = append(out, '_')
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			b == '-', b == '_', b == '.':
			out = append(out, b)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
