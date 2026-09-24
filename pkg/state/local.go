// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// LocalStateStore 是 StateStore 的本地 JSON 文件实现（默认，零回归）。
//
// 落盘路径：<root>/state/<owner>/<type>/<name>.json（name 段内部 "/" 转目录层级）。
// key 三段式 <owner>/<type>/<name>（share/<token>、audit/<seq> 等两段 key 的
// owner 段缺省、type 段作首目录）。
//
// 原子写：tmp（同目录）→ WriteFile → Rename；Rename 失败回退直接写 + Warn 日志
// （对齐既有 checksum/dedup 的 Windows 回退语义）。mu 串行化跨 key 写入
// （Windows 并发 rename 到同一目录树防护，与既有 saveMu 同语义）。
//
// 安全边界：每段过 validSegment（与 storage.ValidSegmentName 同语义，逐字对照见
// key_test.go）；非法 key fail-closed，绝不静默改写。
type LocalStateStore struct {
	root   string // <root>/state 绝对路径
	mu     sync.Mutex
	logger *slog.Logger
	pollNS func() int64 // Watch 轮询间隔（纳秒；默认 500ms，测试可注入）
}

// Option 是 LocalStateStore 构造选项。
type Option func(*LocalStateStore)

// WithPollInterval 注入 Watch 轮询间隔（仅测试使用：生产恒默认 500ms，
// 测试用短间隔 + testutil.WaitFor 条件轮询，满足 R14 禁 time.Sleep 棘轮）。
func WithPollInterval(d time.Duration) Option {
	return func(s *LocalStateStore) { s.pollNS = func() int64 { return int64(d) } }
}

// defaultPollIntervalNS 是 Watch 默认轮询间隔（500ms）。
const defaultPollIntervalNS = 500_000_000

// NewLocalStateStore 创建绑定到 stateDir（<root>/state）的本地状态存储。
// 目录不存在时自动创建（首个 Put 时）；logger nil 回落 slog.Default。
func NewLocalStateStore(stateDir string, logger *slog.Logger, opts ...Option) *LocalStateStore {
	s := &LocalStateStore{
		root:   stateDir,
		logger: slogutil.Default(logger),
		pollNS: func() int64 { return defaultPollIntervalNS },
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s
}

// validateKey 校验 key 三段式并返回目录相对路径（rel 以 "/" 分隔，段间安全）。
//
// 段校验与 pkg/storage.ValidSegmentName 逐字同语义：拒绝空段、`.`/`..`、
// `/`/`\`、Windows 非法字符、`.__` 前缀、保留设备名、尾点/尾空格、长度 > 255。
// name 段（最后一段）内部允许 "/"（checksum rel），逐子段校验后整体作为
// 相对路径的末尾目录树。
func validateKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("state: key 为空")
	}
	if strings.ContainsRune(key, '\x00') {
		return "", fmt.Errorf("state: key 含空字节")
	}
	parts := strings.Split(key, "/")
	// 至少两段（type/name 或 owner/type/name）；单段 key 拒绝（无类型分片）。
	if len(parts) < 2 {
		return "", fmt.Errorf("state: key %q 至少需要两段（<type>/<name> 或 <owner>/<type>/<name>）", key)
	}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("state: key %q 含空段", key)
		}
		if !validSegment(p) {
			return "", fmt.Errorf("state: key %q 含非法段 %q", key, p)
		}
	}
	// 目录相对路径：前 n-1 段为目录，最后一段 + ".json" 为文件。
	dirParts := parts[:len(parts)-1]
	_ = parts[len(parts)-1] // 末段为文件名（去 .json 由 keyPath 追加）
	return filepath.Join(append([]string{}, dirParts...)...), nil
}

// validSegment 复用 pkg/storage.ValidSegmentName 的判定语义（不 import pkg/storage
// ——基础包零内部依赖，见 doc.go；判定对照由 key_test.go 钉住）。
func validSegment(name string) bool {
	if name == "" {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return false
	}
	if strings.HasPrefix(name, ".__") {
		return false
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	if len(name) > 255 {
		return false
	}
	base := name
	if before, _, ok := strings.Cut(name, "."); ok {
		base = before
	}
	if _, ok := windowsReserved[strings.ToUpper(base)]; ok {
		return false
	}
	return true
}

// windowsReserved 是 Windows 保留设备名（基名判定，与 pkg/storage 同集）。
var windowsReserved = map[string]struct{}{
	"CON": {}, "NUL": {}, "PRN": {}, "AUX": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// keyPath 返回 key 的绝对落盘路径（校验失败返回错误）。
func (s *LocalStateStore) keyPath(key string) (string, error) {
	rel, err := validateKey(key)
	if err != nil {
		return "", err
	}
	// 末段文件名：最后一段 + ".json"。
	file := key[strings.LastIndex(key, "/")+1:] + ".json"
	return filepath.Join(s.root, filepath.FromSlash(rel), file), nil
}

// Get 读取 key 的值；不存在返回 ErrKeyNotFound。
// 持 s.mu：Windows 上并发 Rename（Put）与读取目标文件会共享冲突
// （“file being used by another process”），与既有 saveMu 串行化语义一致。
func (s *LocalStateStore) Get(ctx context.Context, key string) ([]byte, error) {
	path, err := s.keyPath(key)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("state: 读取 %s 失败: %w", key, err)
	}
	return data, nil
}

// Put 原子写 key 的值（tmp + rename；Rename 失败回退直接写 + Warn）。
func (s *LocalStateStore) Put(ctx context.Context, key string, data []byte) error {
	path, err := s.keyPath(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeAtomic(path, data)
}

// writeAtomic 是 tmp+rename 原子写实现（调用方须持 s.mu）。
func (s *LocalStateStore) writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("state: 创建目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("state: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 no-op；失败时清理残留
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: 写临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		s.logger.Warn("原子重命名失败，回退到直接写入", "path", path, "error", err)
		if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
			return fmt.Errorf("state: 回退写入失败: %w", writeErr)
		}
	}
	return nil
}

// Delete 删除 key（幂等：不存在静默成功）。
func (s *LocalStateStore) Delete(ctx context.Context, key string) error {
	path, err := s.keyPath(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("state: 删除 %s 失败: %w", key, err)
	}
	return nil
}

// List 返回 prefix 前缀下的完整 key 列表（排序不承诺稳定；调用方自行排序）。
// prefix 为空 = 列出全部；prefix 形如 "checksum/"（目录前缀）。跳过 .tmp 残留。
func (s *LocalStateStore) List(ctx context.Context, prefix string) ([]string, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	walkRoot := s.root
	if prefix != "" {
		// prefix 作为目录前缀：借用 validateKey 段校验（拼探测 key 取目录部分）。
		probeKey := strings.TrimSuffix(prefix, "/") + "/_probe_"
		rel, err := validateKey(probeKey)
		if err != nil {
			return nil, fmt.Errorf("state: 非法前缀 %q: %w", prefix, err)
		}
		walkRoot = filepath.Join(s.root, filepath.Dir(filepath.FromSlash(rel)))
	}
	var out []string
	err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // 前缀目录不存在 = 空
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".tmp-") || !strings.HasSuffix(base, ".json") {
			return nil
		}
		rel, rerr := filepath.Rel(s.root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		key := strings.TrimSuffix(rel, ".json")
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		out = append(out, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("state: List(%q) 失败: %w", prefix, err)
	}
	return out, nil
}

// validatePrefix 校验 List 前缀（空串合法；非空必须以段校验通过）。
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.ContainsRune(prefix, '\x00') {
		return fmt.Errorf("state: 前缀含空字节")
	}
	norm := strings.TrimSuffix(prefix, "/")
	if norm == "" {
		return nil
	}
	if strings.HasPrefix(norm, "/") || strings.Contains(norm, "..") {
		return fmt.Errorf("state: 非法前缀 %q", prefix)
	}
	if _, err := validateKey(norm + "/_probe_"); err != nil {
		return fmt.Errorf("state: 非法前缀 %q: %w", prefix, err)
	}
	return nil
}

// CAS 原子比较并交换：当前值 == old 时替换为 new。
//
// 语义（对齐设计 §2.2）：old == nil 表示「期望不存在」（create-only）；new == nil
// 表示「期望删除」；不匹配返回 ErrCASMismatch。
// 实现：持 s.mu（串行化全部写）下读当前 → 比对 → 写/删——原子，无读-改-写非原子序列。
func (s *LocalStateStore) CAS(ctx context.Context, key string, old, new []byte) error {
	path, err := s.keyPath(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("state: CAS 读 %s 失败: %w", key, err)
		}
		cur = nil
	}
	// old == nil → 期望不存在：当前必须不存在。
	if old == nil {
		if cur != nil {
			return ErrCASMismatch
		}
	} else if !equalBytes(cur, old) {
		return ErrCASMismatch
	}
	// new == nil → 删除；否则原子写。
	if new == nil {
		if cur == nil {
			return nil // 幂等删除
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("state: CAS 删除 %s 失败: %w", key, err)
		}
		return nil
	}
	return s.writeAtomic(path, new)
}

// equalBytes 字节比较（nil 与空切片视为相等——语义：文件不存在 = 空值）。
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Watch 订阅 prefix 下的变更流（后台轮询 diff，默认 500ms；测试可注入短间隔）。
// 初始不重放现有键；ctx 取消时关闭通道并返回。
func (s *LocalStateStore) Watch(ctx context.Context, prefix string) (<-chan Change, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	ch := make(chan Change)
	go s.watchLoop(ctx, prefix, ch)
	return ch, nil
}

// watchLoop 轮询 List(prefix) 与上次快照 diff，推 put/delete 变更。
func (s *LocalStateStore) watchLoop(ctx context.Context, prefix string, ch chan<- Change) {
	defer close(ch)
	interval := time.Duration(s.pollNS())
	last := map[string]bool{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			keys, err := s.List(ctx, prefix)
			if err != nil {
				continue // 底层不可用：跳过本轮（调用方退避语义）
			}
			cur := map[string]bool{}
			for _, k := range keys {
				cur[k] = true
			}
			for k := range cur {
				if !last[k] {
					select {
					case ch <- Change{Key: k, Op: "put"}:
					case <-ctx.Done():
						return
					}
				}
			}
			for k := range last {
				if !cur[k] {
					select {
					case ch <- Change{Key: k, Op: "delete"}:
					case <-ctx.Done():
						return
					}
				}
			}
			last = cur
		}
	}
}
