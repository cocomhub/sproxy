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
	logger.DebugContext(ctx, "JSONKVStore 已创建", "dir", dir)
	return &JSONKVStore{dir: dir, logger: logger}, nil
}

func (s *JSONKVStore) Save(ctx context.Context, key string, value map[string]any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	path := filepath.Join(s.dir, key+".json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("重命名文件失败: %w", err)
	}
	return nil
}

func (s *JSONKVStore) Load(ctx context.Context, key string) (map[string]any, error) {
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
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (s *JSONKVStore) Delete(ctx context.Context, key string) error {
	path := filepath.Join(s.dir, key+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("删除缓存文件失败: %w", err)
	}
	return nil
}

func (s *JSONKVStore) Close() error { return nil }
