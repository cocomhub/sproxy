// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// StateStoreConfig 是状态存储后端的通用配置。
// Type 为 local（缺省，零回归）；mongo/raft 为插件位（raft 未实现，装配报错）。
type StateStoreConfig struct {
	Type string // local | mongo | raft；空 = local（SetDefaults 语义）
	Dir  string // local 专属：状态根目录（<root>/state）
	// Mongo 为 mongo 实现配置（F3 片；本期不消费）。
	Mongo MongoConfig `yaml:"mongo" mapstructure:"mongo"`
}

// MongoConfig 是 mongo 后端的连接配置（F3 片使用，仿设计 §2.5）。
type MongoConfig struct {
	URI        string
	Database   string
	Collection string
}

// StateStoreFactory 按配置构造状态存储（配置校验失败返回错误，装配期 fail-fast）。
type StateStoreFactory func(cfg StateStoreConfig, logger *slog.Logger) (StateStore, error)

// ErrStateStoreNotRegistered 是 NewStateStore 分派未注册类型的哨兵错误（fail-closed，
// 不回落 local——回落会造成「以为多节点一致、实际各写各的」的最坏情况）。
var ErrStateStoreNotRegistered = errors.New("state: backend type not registered")

// ErrStateStoreNotImplemented 是 raft 等预留但未实现类型的装配错误。
var ErrStateStoreNotImplemented = errors.New("state: backend type not implemented yet")

var (
	stateMu        sync.RWMutex
	stateFactories = map[string]StateStoreFactory{}
)

// RegisterStateStore 注册状态存储类型构造器（可插拔扩展，仿
// pkg/volume/registry.RegisterBackend 模式）。
//
// 重复注册同一类型 → panic（编程错误：两个包声明了同一类型的所有权，装配期应
// fail-fast 暴露而非静默覆盖）；空类型名 → panic（空串 = local 保留名）。
func RegisterStateStore(typ string, f StateStoreFactory) {
	if typ == "" || typ == "local" {
		panic(fmt.Sprintf("state: 非法类型 %q（空串与 %q 保留给本地实现）", typ, "local"))
	}
	if f == nil {
		panic(fmt.Sprintf("state: 类型 %q 的构造器为 nil", typ))
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if _, dup := stateFactories[typ]; dup {
		panic(fmt.Sprintf("state: 类型 %q 重复注册", typ))
	}
	stateFactories[typ] = f
}

// NewStateStore 按类型分派构造状态存储。
//
// 未注册的 Type → 明确错误（fail-closed，不回落 local）。type 空串/"local" 直接构造
// LocalStateStore（内置实现，无需注册表）；"raft" 为预留位 → 响亮「未实现」。
func NewStateStore(typ string, cfg StateStoreConfig, logger *slog.Logger) (StateStore, error) {
	if typ == "" || typ == "local" {
		dir := cfg.Dir
		if dir == "" {
			return nil, fmt.Errorf("state: local 实现需要 Dir（<root>/state）")
		}
		return NewLocalStateStore(dir, logger), nil
	}
	if typ == "raft" {
		return nil, fmt.Errorf("%w: raft（etcd/raft 复制状态机，roadmap 11.12-F5）", ErrStateStoreNotImplemented)
	}
	stateMu.RLock()
	f, ok := stateFactories[typ]
	stateMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q（需 RegisterStateStore 注册）", ErrStateStoreNotRegistered, typ)
	}
	st, err := f(cfg, slogutil.Default(logger))
	if err != nil {
		return nil, fmt.Errorf("state: 类型 %q 构造失败: %w", typ, err)
	}
	if st == nil {
		return nil, fmt.Errorf("state: 类型 %q 构造器返回 nil（装配错误）", typ)
	}
	return st, nil
}

// StateStoreTypes 返回已注册类型列表（副本，顺序不承诺稳定）。
func StateStoreTypes() []string {
	stateMu.RLock()
	defer stateMu.RUnlock()
	if len(stateFactories) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(stateFactories))
	for typ := range stateFactories {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// UnregisterStateStoreForTest 移除测试注册的类型（测试辅助；生产代码不得调用）。
func UnregisterStateStoreForTest(typ string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	delete(stateFactories, typ)
}
