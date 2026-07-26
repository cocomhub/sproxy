// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/plugin"
)

// KVStore 通用键值存储接口。
type KVStore interface {
	Save(ctx context.Context, key string, value map[string]any) error
	Load(ctx context.Context, key string) (map[string]any, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
	Close() error
}

// StructCodec 在 struct（带 json tag）和 map[string]any 之间转换。
type StructCodec struct{}

func (StructCodec) ToMap(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("结构体序列化失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("JSON 解码为 map 失败: %w", err)
	}
	return m, nil
}

func (StructCodec) FromMap(m map[string]any, v any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("map 序列化失败: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("JSON 解码到结构体失败: %w", err)
	}
	return nil
}

// KVStoreRegistry 是可插拔的 KVStore 注册表。
var KVStoreRegistry = plugin.New[KVStoreFactory]("kv_store", &jsonKVStoreFactory{})

// KVStoreFactory 是 KVStore 工厂接口。
type KVStoreFactory interface {
	Name() string
	Open(ctx context.Context, cfg map[string]string) (KVStore, error)
}

type jsonKVStoreFactory struct{}

func (f *jsonKVStoreFactory) Name() string { return "json" }

func (f *jsonKVStoreFactory) Open(ctx context.Context, cfg map[string]string) (KVStore, error) {
	dir := cfg["dir"]
	if dir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("获取用户缓存目录失败: %w", err)
		}
		dir = filepath.Join(cacheDir, "sproxy", "kvstore")
	}
	return NewJSONKVStore(ctx, dir, slog.Default())
}

// MemoryKVStore 内存 KVStore 实现（用于测试）。
type MemoryKVStore struct {
	mu   sync.RWMutex
	data map[string]map[string]any
}

func NewMemoryKVStore() *MemoryKVStore {
	return &MemoryKVStore{data: make(map[string]map[string]any)}
}

func (s *MemoryKVStore) Save(_ context.Context, key string, value map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := make(map[string]any, len(value))
	maps.Copy(clone, value)
	s.data[key] = clone
	return nil
}

func (s *MemoryKVStore) Load(_ context.Context, key string) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return nil, fmt.Errorf("key not found: %s", key)
	}
	clone := make(map[string]any, len(v))
	maps.Copy(clone, v)
	return clone, nil
}

func (s *MemoryKVStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for k := range s.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (s *MemoryKVStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *MemoryKVStore) Close() error { return nil }
