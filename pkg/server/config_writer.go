// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// config_writer.go 是「运行时用户卷 ↔ config 声明」的写回能力（C4，2026-10-06 用户裁决）：
//
//   - /api/volumes/user 创建卷成功后，把新卷追加到 config 文件 `volumes:` 段（重启后卷不丢）；
//   - 删除卷成功后，从 config 文件的 `volumes:` 段移除；
//   - config 写回失败 → 创建侧回滚（Set/store/links/配额一并回滚，保持一致性）；删除侧告警
//     （store 已删无法无损回滚，记录取舍）。
//
// 实现（FileConfigWriter）用 gopkg.in/yaml.v3 读改写（仓库唯一第三方依赖），保持 UTF-8 无
// BOM；原子写（临时文件 + fsync + rename）。round-trip 不保留注释/锚点（yaml.v3 语义），
// 配置语义（volumes 声明序、默认卷为首卷）由列表顺序保留。

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ConfigWriter 是「运行时用户卷写回 config 文件」的能力接口。实现由装配层注入
// （main 持有 config 路径）；nil = 未注入（无 config 文件语义的装配/测试），写回跳过。
type ConfigWriter interface {
	// AppendVolume 把用户卷追加到 config 文件 volumes 段（已存在同卷名 → 原地更新防重复）。
	AppendVolume(v UserVolume) error
	// RemoveVolume 从 config 文件 volumes 段移除指定卷（不存在 → no-op 成功）。
	RemoveVolume(name string) error
}

// FileConfigWriter 是 ConfigWriter 的 YAML 文件实现（gopkg.in/yaml.v3）。
// path 为 config 文件路径（main 的 cfgFile）。
type FileConfigWriter struct {
	path string
}

// NewFileConfigWriter 构造以 config 文件路径为目标的写回器。
func NewFileConfigWriter(path string) *FileConfigWriter {
	return &FileConfigWriter{path: path}
}

// AppendVolume 实现 ConfigWriter。
func (w *FileConfigWriter) AppendVolume(v UserVolume) error {
	if w == nil || w.path == "" {
		return nil
	}
	root, err := w.load()
	if err != nil {
		return err
	}
	vols, _ := root["volumes"].([]any)
	entry := w.volumeEntry(v)
	for i, item := range vols {
		if m, ok := item.(map[string]any); ok {
			if name, _ := m["name"].(string); name == v.Name {
				vols[i] = entry
				root["volumes"] = vols
				return w.save(root)
			}
		}
	}
	root["volumes"] = append(vols, entry)
	return w.save(root)
}

// RemoveVolume 实现 ConfigWriter。
func (w *FileConfigWriter) RemoveVolume(name string) error {
	if w == nil || w.path == "" {
		return nil
	}
	root, err := w.load()
	if err != nil {
		return err
	}
	vols, _ := root["volumes"].([]any)
	out := make([]any, 0, len(vols))
	removed := false
	for _, item := range vols {
		if m, ok := item.(map[string]any); ok {
			if n, _ := m["name"].(string); n == name {
				removed = true
				continue
			}
		}
		out = append(out, item)
	}
	if !removed {
		return nil // 不存在 → no-op（卷名笔误/已移除）
	}
	root["volumes"] = out
	return w.save(root)
}

// volumeEntry 把 UserVolume 映射为 config `volumes:` 段条目（只写非空字段）。
// capacity 写为 vol_capacity（ByteSize 纯数字字节，yaml.v3 对命名 int 走数值）。
func (w *FileConfigWriter) volumeEntry(v UserVolume) map[string]any {
	entry := map[string]any{"name": v.Name, "type": v.Type}
	if v.Capacity > 0 {
		entry["vol_capacity"] = v.Capacity
	}
	if len(v.Extra) > 0 {
		entry["extra"] = v.Extra
	}
	return entry
}

// load 读 config 文件为 map（不存在 → 空 map，新建写回）。
func (w *FileConfigWriter) load() (map[string]any, error) {
	data, err := os.ReadFile(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("config 写回：读取 %s 失败: %w", w.path, err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("config 写回：解析 %s 失败: %w", w.path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

// save 原子写回（临时文件 + fsync + rename），UTF-8 无 BOM。
func (w *FileConfigWriter) save(root map[string]any) error {
	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("config 写回：序列化失败: %w", err)
	}
	dir := filepath.Dir(w.path)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return fmt.Errorf("config 写回：创建目录 %s 失败: %w", dir, mkErr)
	}
	tmp, err := os.CreateTemp(dir, "*.sproxy-config.tmp")
	if err != nil {
		return fmt.Errorf("config 写回：创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config 写回：写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config 写回：fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config 写回：关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, w.path); err != nil {
		return fmt.Errorf("config 写回：原子重命名失败: %w", err)
	}
	return nil
}
