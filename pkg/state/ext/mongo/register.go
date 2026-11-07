// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mongo

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/cocomhub/sproxy/pkg/state"
)

// RegisterStateBackend 注册 "mongo" 类型到 pkg/state 注册表（装配层显式调用）。
//
// 幂等：重复调用 no-op（防 cmd/sproxy 装配 + 测试同时注册触发重复 panic）。
//
// 与 StateStoreConfig.Mongo（state_store.mongo.*）对接：工厂从 cfg.Mongo 取
// uri/database/collection 构造 MongoStateStore；uri 空 / 连接不可达 → 装配期
// fail-closed 报错（不回落 local——防「以为多节点一致、实际各写各的」）。
var registerOnce sync.Once

func RegisterStateBackend() {
	registerOnce.Do(func() {
		state.RegisterMongoStateStore(func(cfg state.StateStoreConfig, logger *slog.Logger) (state.StateStore, error) {
			if cfg.Mongo.URI == "" {
				return nil, fmt.Errorf("mongo: state_store.mongo.uri 必填（type=mongo 配置缺失，fail-closed）")
			}
			st, err := NewMongoStateStore(cfg.Mongo.URI, cfg.Mongo.Database, cfg.Mongo.Collection, logger)
			if err != nil {
				return nil, err
			}
			return st, nil
		})
	})
}
