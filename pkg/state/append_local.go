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

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// LocalAppendStore 是 AppendStore 的本地 append-only JSON lines 实现（审计/事件）。
//
// 落盘：<root>/state/<key 目录>/<name>.json，O_APPEND|O_CREATE 单次 Write 追加一行
// （对齐 pkg/server/audit_store.go 的 appendLine 语义：原子 append、不覆盖历史）。
// 失败返回 error（调用方按「尽力而为」语义记日志不阻断业务，见设计 §3.3）。
type LocalAppendStore struct {
	root   string // <root>/state
	logger *slog.Logger
}

// NewLocalAppendStore 创建绑定到 stateDir（<root>/state）的本地追加存储。
func NewLocalAppendStore(stateDir string, logger *slog.Logger) *LocalAppendStore {
	return &LocalAppendStore{root: stateDir, logger: slogutil.Default(logger)}
}

// Append 把 data 作为一行追加到 key 的尾部（JSON lines；data 不应含换行——
// 审计事件为单行 JSON，调用方负责序列化）。
func (s *LocalAppendStore) Append(ctx context.Context, key string, data []byte) error {
	rel, err := validateKey(key)
	if err != nil {
		return err
	}
	// 末段文件名：最后一段 + ".json"。
	file := key[strings.LastIndex(key, "/")+1:] + ".json"
	path := filepath.Join(s.root, filepath.FromSlash(rel), file)
	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return fmt.Errorf("state: 追加创建目录失败: %w", mkErr)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("state: 追加打开 %s 失败: %w", key, err)
	}
	defer func() { _ = f.Close() }()
	line := append(append([]byte(nil), data...), '\n')
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("state: 追加写 %s 失败: %w", key, err)
	}
	return nil
}
