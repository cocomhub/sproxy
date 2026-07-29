// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Phase 常量
const (
	PhaseSubmitting  = "submitting"
	PhaseWaiting     = "waiting"
	PhaseArchiving   = "archiving"
	PhaseDownloading = "downloading"
	PhaseCleaning    = "cleaning"
	PhaseCompleted   = "completed"
	PhaseFailed      = "failed"
)

// Status 常量
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// ChainRunner 链式操作执行器接口。
type ChainRunner interface {
	ID() string
	Phase() string
	Status() string
	Run(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error
	State() map[string]any
	Restore(state map[string]any) error
}

// ChainResult 链式操作结果。
type ChainResult struct {
	ChainID string      `json:"chain_id"`
	Phase   string      `json:"phase"`
	Status  string      `json:"status"`
	Error   string      `json:"error,omitempty"`
	Raw     ChainRunner `json:"-"` // 原始 runner
}

// chainOptions 链式操作选项。
type chainOptions struct {
	pollInterval time.Duration
	timeout      time.Duration
	keepFiles    bool
	progressFn   func(ctx context.Context, phase string, msg string, current, total int)
}

// ChainOption 链式操作选项函数。
type ChainOption func(*chainOptions)

func WithChainPollInterval(d time.Duration) ChainOption {
	return func(o *chainOptions) {
		if d > 0 {
			o.pollInterval = d
		}
	}
}

func WithChainTimeout(d time.Duration) ChainOption {
	return func(o *chainOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

func WithChainKeepFiles() ChainOption {
	return func(o *chainOptions) {
		o.keepFiles = true
	}
}

func WithChainProgress(fn func(ctx context.Context, phase string, msg string, current, total int)) ChainOption {
	return func(o *chainOptions) {
		o.progressFn = fn
	}
}

func defaultChainOptions() chainOptions {
	return chainOptions{
		pollInterval: 3 * time.Second,
		timeout:      30 * time.Minute,
		keepFiles:    false,
	}
}

// ChainManager 链式操作管理器。
type ChainManager struct {
	store KVStore
	codec StructCodec
}

func NewChainManager(store KVStore) *ChainManager {
	return &ChainManager{store: store, codec: StructCodec{}}
}

// Run 执行链式操作（自动持久化，支持恢复）。
func (m *ChainManager) Run(ctx context.Context, runner ChainRunner) error {
	return m.RunWithProgress(ctx, runner, nil)
}

// RunWithProgress 执行链式操作，并支持外部进度回调。
func (m *ChainManager) RunWithProgress(ctx context.Context, runner ChainRunner, progressFn func(ctx context.Context, phase string, msg string, current, total int)) error {
	m.saveState(ctx, runner)
	reportFn := func(ctx context.Context, phase string, msg string, current, total int) {
		m.saveState(ctx, runner)
		if progressFn != nil {
			progressFn(ctx, phase, msg, current, total)
		}
	}
	err := runner.Run(ctx, reportFn)
	if err != nil {
		state := runner.State()
		state["status"] = StatusFailed
		_ = m.store.Save(ctx, "chain:"+runner.ID(), state)
		return err
	}
	_ = m.store.Delete(ctx, "chain:"+runner.ID())
	return nil
}

func (m *ChainManager) Resume(ctx context.Context, chainID string) (ChainRunner, error) {
	state, err := m.store.Load(ctx, "chain:"+chainID)
	if err != nil {
		return nil, fmt.Errorf("加载链状态失败: %w", err)
	}
	runner, err := resolveRunner(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("解析 runner 类型失败: %w", err)
	}
	if err := runner.Restore(state); err != nil {
		return nil, fmt.Errorf("恢复 runner 状态失败: %w", err)
	}
	return runner, nil
}

func (m *ChainManager) List(ctx context.Context) ([]ChainRunner, error) {
	keys, err := m.store.List(ctx, "chain:")
	if err != nil {
		return nil, err
	}
	var runners []ChainRunner
	for _, key := range keys {
		state, err := m.store.Load(ctx, key)
		if err != nil {
			continue
		}
		status, _ := state["status"].(string)
		if status == StatusCompleted || status == StatusFailed {
			continue
		}
		runner, err := resolveRunner(ctx, state)
		if err != nil {
			continue
		}
		if err := runner.Restore(state); err != nil {
			continue
		}
		runners = append(runners, runner)
	}
	return runners, nil
}

func (m *ChainManager) Delete(ctx context.Context, chainID string) error {
	return m.store.Delete(ctx, "chain:"+chainID)
}

func (m *ChainManager) saveState(ctx context.Context, runner ChainRunner) {
	state := runner.State()
	_ = m.store.Save(ctx, "chain:"+runner.ID(), state)
}

// runnerRegistry 是 ChainRunner 类型注册表。
var (
	runnerRegistryMu sync.RWMutex
	runnerRegistry   = map[string]func() ChainRunner{}
)

func RegisterRunner(typeName string, factory func() ChainRunner) {
	runnerRegistryMu.Lock()
	defer runnerRegistryMu.Unlock()
	runnerRegistry[typeName] = factory
}

func resolveRunner(ctx context.Context, state map[string]any) (ChainRunner, error) {
	typeName, ok := state["type"].(string)
	if !ok {
		return nil, fmt.Errorf("state 缺少 type 字段")
	}
	runnerRegistryMu.RLock()
	factory, ok := runnerRegistry[typeName]
	runnerRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("未知的 runner 类型: %s", typeName)
	}
	return factory(), nil
}
