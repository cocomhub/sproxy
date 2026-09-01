// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"fmt"
)

// JSONStore 提供类型安全的原子 JSON 记录读写。
// 底层 Store 负责原子写；本层只做 JSON 编解码。
type JSONStore[T any] struct {
	s Store
}

// NewJSON 在给定的字节 KV 之上创建 JSON 记录存储。
func NewJSON[T any](s Store) *JSONStore[T] {
	return &JSONStore[T]{s: s}
}

// Get 读取 key 对应的 JSON 记录并解码。
// key 不存在时返回 nil 与 os.ErrNotExist（可用 errors.Is 判断）。
func (j *JSONStore[T]) Get(key string) (*T, error) {
	data, err := j.s.Get(key)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("store: JSON 解析 %q 失败: %w", key, err)
	}
	return &v, nil
}

// Set 将记录编码为 JSON 并原子写入。
// v 为 nil 时返回错误，避免持久化 "null" 记录。
func (j *JSONStore[T]) Set(key string, v *T) error {
	if v == nil {
		return errors.New("store: Set 不允许 nil 记录")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return j.s.Set(key, data)
}

// Delete 删除 key 对应的记录（幂等：不存在时返回 nil）。
func (j *JSONStore[T]) Delete(key string) error {
	return j.s.Delete(key)
}

// List 返回指定前缀下的全部记录（解码后的对象）；空前缀 = 全部。
func (j *JSONStore[T]) List(prefix string) ([]*T, error) {
	items, err := j.s.List(prefix)
	if err != nil {
		return nil, err
	}
	out := make([]*T, 0, len(items))
	for _, data := range items {
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("store: JSON 解析失败: %w", err)
		}
		out = append(out, &v)
	}
	return out, nil
}
