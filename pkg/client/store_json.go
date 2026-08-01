// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// JSONKVStore 基于 JSON 文件的 KVStore 实现。
// 每个 key 对应一个 .json 文件，写入使用原子写（tmp + rename）。
type JSONKVStore struct {
	dir    string
	logger *slog.Logger
}

func NewJSONKVStore(ctx context.Context, dir string, logger *slog.Logger) (*JSONKVStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}
	// 启动时清理残留的 .tmp.json 文件（上次异常退出留下的）
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp.json") {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					logger.WarnContext(ctx, "清理残留临时文件失败", "file", entry.Name(), "error", err)
				}
			}
		}
	}
	logger.DebugContext(ctx, "JSONKVStore 已创建", "dir", dir)
	return &JSONKVStore{dir: dir, logger: logger}, nil
}

// validateKey 校验 key 合法性，防止路径注入
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("key 不能为空")
	}
	if strings.Contains(key, "..") || strings.Contains(key, "/") || strings.Contains(key, "\\") {
		return fmt.Errorf("key 包含非法路径字符: %q", key)
	}
	return nil
}

func (s *JSONKVStore) Save(ctx context.Context, key string, value map[string]any) error {
	if err := validateKey(key); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	path := filepath.Join(s.dir, key+".json")
	tmpPath := filepath.Join(s.dir, key+".tmp.json")
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("重命名文件失败: %w", err)
	}
	return nil
}

func (s *JSONKVStore) Load(ctx context.Context, key string) (map[string]any, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取缓存文件失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析缓存文件失败: %w", err)
	}
	return m, nil
}

func (s *JSONKVStore) List(ctx context.Context, prefix string) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取缓存目录失败: %w", err)
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp.json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		// 过滤掉 .tmp.json 和 .json.tmp 模式的残留文件
		if strings.HasSuffix(key, ".tmp") {
			continue
		}
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (s *JSONKVStore) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	path := filepath.Join(s.dir, key+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("删除缓存文件失败: %w", err)
	}
	// 同时清理残留的 .tmp.json 文件
	tmpPath := filepath.Join(s.dir, key+".tmp.json")
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		s.logger.WarnContext(ctx, "清理临时文件失败", "key", key, "error", err)
	}
	return nil
}

func (s *JSONKVStore) Close() error { return nil }