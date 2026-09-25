// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package migrate 提供迁移向导的纯逻辑层（roadmap 11.7-⑩）：
//
//	Manifest：导出清单（schema:1 + server 引用 + 文件条目，校验/原子读写）；
//	Exporter：源机递归列表 + 下载到 <out>/files/（相对路径保持）+ 校验 + 写 manifest；
//	Importer：读 manifest → 逐文件上传 → 目标 stat/checksum 校验（幂等/冲突分类）；
//	MirrorConfig：由 manifest + 目标机卷列表生成 volumes[].mirror_to / sync_remotes
//	/ federation 片段 YAML（--print 或 --write）。
//
// 纯逻辑（网络经 *client.FileClient 注入）——双 mock server 端到端单测零服务端改动。

package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SchemaVersion 是当前清单 schema 版本。
const SchemaVersion = 1

// ErrSchemaMismatch 表示 manifest schema 与本实现不兼容（拒绝导入，防静默错迁）。
var ErrSchemaMismatch = errors.New("manifest schema 不兼容")

// ServerRef 记录导出源机引用（诊断/镜像配置生成用）。
type ServerRef struct {
	URL string `json:"url"`
}

// FileEntry 是 manifest 中单个文件条目（导出端记录，导入端校验）。
type FileEntry struct {
	Name     string `json:"name"`             // 服务端相对路径（ToSlash）
	Size     int64  `json:"size"`             // 字节数
	Checksum string `json:"checksum"`         // SHA-256 hex
	MTime    int64  `json:"mtime"`            // UnixNano
	Volume   string `json:"volume,omitempty"` // 源卷名（多卷导出；空 = auto）
}

// Manifest 是迁移清单（导出端原子写，导入端 fail-closed 自检）。
type Manifest struct {
	Schema     int         `json:"schema"`
	Server     ServerRef   `json:"server"`
	ExportedAt time.Time   `json:"exported_at"`
	Files      []FileEntry `json:"files"`
}

// Validate 校验清单可导入性（fail-closed）：
// schema 必须匹配；每个条目文件名非空且无路径穿越、checksum 非空。
func (m *Manifest) Validate() error {
	if m.Schema != SchemaVersion {
		return fmt.Errorf("%w: got %d want %d", ErrSchemaMismatch, m.Schema, SchemaVersion)
	}
	for i := range m.Files {
		f := &m.Files[i]
		if f.Name == "" {
			return fmt.Errorf("文件条目 %d: 文件名为空", i)
		}
		if strings.Contains(f.Name, "..") {
			return fmt.Errorf("文件条目 %q: 路径穿越", f.Name)
		}
		if f.Checksum == "" {
			return fmt.Errorf("文件条目 %q: checksum 为空", f.Name)
		}
	}
	return nil
}

// WriteFile 原子写 manifest.json（tmp + Rename，失败不残留半成品）。
func WriteFile(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("重命名 manifest: %w", err)
	}
	return nil
}

// ReadFile 读并校验 manifest（损坏/schema 不符 → 拒绝，防静默错迁）。
func ReadFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析 manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
