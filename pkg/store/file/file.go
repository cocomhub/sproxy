// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package file 提供基于文件的 Store 实现（原子写：tmp + rename + 锁）。
// key 以 / 分隔；磁盘路径 = Root + 相对 key（拒绝绝对路径、.. 段、空段等逃逸 key）。
package file

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/store"
)

// FileStore 是基于文件系统的字节 KV 实现。
// 每个 key 映射到 Root 下的一个文件；Set 原子写（tmp + rename，持 saveMu 串行化）。
type FileStore struct {
	root   string // 绝对存储根
	saveMu sync.Mutex
}

func init() {
	store.Register("file", New)
}

// New 打开文件存储根。cfg.Root 为空返回错误；目录不存在时自动创建。
// key 映射：磁盘路径 = Root + 相对 key；key 必须通过安全校验（拒绝 ../、绝对路径、空段）。
func New(cfg store.StoreConfig) (store.Store, error) {
	root := cfg.Root
	if root == "" {
		return nil, errors.New("store/file: Root 不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("store/file: 解析 Root 失败: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("store/file: 创建 Root 失败: %w", err)
	}
	return &FileStore{root: abs}, nil
}

// Get 读取 key 对应的值；key 不存在时返回 os.ErrNotExist。
func (f *FileStore) Get(key string) ([]byte, error) {
	path, err := f.keyPath(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}

// Set 原子写入 key 的值：先写 <key>.tmp 再 os.Rename。
// 持 saveMu 串行化写入（Windows 并发 rename 防护）；Rename 失败回退直接覆盖写。
func (f *FileStore) Set(key string, value []byte) error {
	path, err := f.keyPath(key)
	if err != nil {
		return err
	}
	f.saveMu.Lock()
	defer f.saveMu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store/file: 创建父目录失败: %w", err)
	}
	tmp := path + ".tmp"
	// 兜底清理：Rename 成功后 tmp 已不在原位，os.Remove 会无声失败；Rename 失败时真正清除残留。
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, value, 0o644); err != nil {
		return fmt.Errorf("store/file: 写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		if writeErr := os.WriteFile(path, value, 0o644); writeErr != nil {
			return fmt.Errorf("store/file: 原子重命名失败且回退写入失败: %w", writeErr)
		}
	}
	return nil
}

// Delete 删除 key 对应的值（幂等：key 不存在时返回 nil）。
func (f *FileStore) Delete(key string) error {
	path, err := f.keyPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store/file: 删除失败: %w", err)
	}
	return nil
}

// List 返回前缀 prefix 下的全部值。
// 遍历 prefix 对应目录（相对 root 的 key 目录），递归收集文件并跳过 .tmp 残留；
// prefix 为空 = 遍历整个 root。值按磁盘字典序返回（不含 key）。
func (f *FileStore) List(prefix string) ([][]byte, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	walkRoot := f.root
	norm := strings.TrimSuffix(prefix, "/")
	if norm != "" {
		walkRoot = filepath.Join(f.root, filepath.FromSlash(norm))
	}
	if _, err := os.Stat(walkRoot); err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 目录不存在 = 空结果
		}
		return nil, fmt.Errorf("store/file: 检查前缀目录失败: %w", err)
	}

	// 先收集匹配的 key，遍历结束后统一读取（避免在 WalkDir 回调内做文件 I/O，防 TOCTOU 穿越）。
	var keys []string
	err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp") {
			return nil // 跳过崩溃残留
		}
		rel, relErr := filepath.Rel(f.root, path)
		if relErr != nil {
			return relErr
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store/file: 前缀遍历失败: %w", err)
	}

	out := make([][]byte, 0, len(keys))
	for _, key := range keys {
		path, keyErr := f.keyPath(key) // 再校验一次（防逃逸兜底）
		if keyErr != nil {
			return nil, keyErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("store/file: 读取 %q 失败: %w", key, readErr)
		}
		out = append(out, data)
	}
	return out, nil
}

// Close 释放资源。file 实现无打开句柄，直接返回 nil。
func (f *FileStore) Close() error {
	return nil
}

// keyPath 校验 key 并返回其绝对磁盘路径（含 root 逃逸兜底检查）。
func (f *FileStore) keyPath(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	path := filepath.Join(f.root, filepath.FromSlash(key))
	// 兜底：确认最终路径仍在 root 之下（防平台 Join 语义差异与未来校验回归）。
	rel, err := filepath.Rel(f.root, path)
	if err != nil {
		return "", fmt.Errorf("store/file: key 路径解析失败: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("store/file: key 逃逸存储根: %q", key)
	}
	return path, nil
}

// validateKey 校验记录 key：拒绝空、绝对路径、空段、. / .. 段、段内反斜杠。
func validateKey(key string) error {
	if key == "" {
		return errors.New("store/file: 空 key 不允许")
	}
	return validateSegments(key)
}

// validatePrefix 校验 List 前缀：空前缀合法（遍历整根），其余同 key 校验。
// 显式拒绝绝对前缀（/ 或 \ 开头），避免 TrimSuffix 后落入空串的模糊语义。
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasPrefix(prefix, "/") || strings.HasPrefix(prefix, `\`) {
		return fmt.Errorf("store/file: 不允许绝对路径前缀: %q", prefix)
	}
	return validateSegments(strings.TrimSuffix(prefix, "/"))
}

// validateSegments 校验协议路径的每个段。
// 最后一段不允许以 .tmp 结尾（.tmp 是临时文件保留后缀，避免记录与临时文件命名空间冲突）。
func validateSegments(p string) error {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return fmt.Errorf("store/file: 不允许绝对路径: %q", p)
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == "" {
			return fmt.Errorf("store/file: 不允许空段: %q", p)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("store/file: 不允许段 %q: %q", seg, p)
		}
		if strings.ContainsAny(seg, `\`) {
			return fmt.Errorf("store/file: 段内不允许反斜杠: %q", p)
		}
		if i == len(segs)-1 && strings.HasSuffix(seg, ".tmp") {
			return fmt.Errorf("store/file: 最后一段不允许以 .tmp 结尾（保留后缀）: %q", p)
		}
	}
	return nil
}
