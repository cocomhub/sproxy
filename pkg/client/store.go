// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("JSON 解码为 map 失败: %w", err)
	}
	return m, nil
}

func (StructCodec) FromMap(m map[string]any, v any) error {
	// 将 map 中的 json.Number 转回原始数值类型
	m = ConvertNumbers(m)
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("map 序列化失败: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("JSON 解码到结构体失败: %w", err)
	}
	return nil
}

// ConvertNumbers 递归将 map 中的 json.Number 转为 int64 或 float64
func ConvertNumbers(m map[string]any) map[string]any {
	result := make(map[string]any, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case json.Number:
			if i, err := val.Int64(); err == nil {
				result[k] = i
			} else if f, err := val.Float64(); err == nil {
				result[k] = f
			} else {
				result[k] = val.String()
			}
		case map[string]any:
			result[k] = ConvertNumbers(val)
		default:
			result[k] = v
		}
	}
	return result
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
	if s.data == nil {
		return fmt.Errorf("store is closed")
	}
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

func (s *MemoryKVStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	return nil
}
