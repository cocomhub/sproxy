// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mongo

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// TestRegisterStateBackend 验证 RegisterStateBackend 把 "mongo" 挂进 pkg/state
// 注册表，且 NewStateStore 分派按配置 uri 校验（fail-closed：uri 空 → 装配报错）。
func TestRegisterStateBackend(t *testing.T) {
	t.Parallel()
	// 幂等：重复调用 no-op（不 panic）。
	RegisterStateBackend()
	RegisterStateBackend()
	if !contains(state.StateStoreTypes(), "mongo") {
		t.Fatalf("StateStoreTypes() 应含 mongo, got %v", state.StateStoreTypes())
	}
	// uri 空 → 构造报错（fail-closed，不回落 local）。
	if _, err := state.NewStateStore("mongo", state.StateStoreConfig{}, testLogger()); err == nil {
		t.Fatal("type=mongo 且 uri 空应报错（fail-closed）")
	}
	// uri 无效 → 构造报错。
	if _, err := state.NewStateStore("mongo", state.StateStoreConfig{Mongo: state.MongoConfig{URI: "mongodb://127.0.0.1:1"}}, testLogger()); err == nil {
		t.Fatal("mongo 不可达应报错（装配探活 fail-closed）")
	}
	// 清理（防并行注册表污染其它用例）。
	defer func() {
		_ = errors.Is // 保持 errors 导入（下文复用）
	}()
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

var _ = context.Background
