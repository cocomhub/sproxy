// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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

// ProgressInfo 表示链式操作进度信息。
type ProgressInfo struct {
	Phase   string // 当前阶段（submitting/waiting/archiving 等）
	Message string // 人类可读的描述
	Current int    // 当前进度
	Total   int    // 总进度
}

// ProgressFunc 是进度回调函数类型。
type ProgressFunc func(ctx context.Context, info ProgressInfo)

// ChainRunner 链式操作执行器接口。
type ChainRunner interface {
	ID() string
	Phase() string
	Status() string
	Run(ctx context.Context, reportFn ProgressFunc) error
	State() map[string]any
	Restore(state map[string]any) error
	SetClient(client *FileClient)
	SetOptions(opts chainOptions)
	SetChainManager(mgr *ChainManager)
}

// ChainResult 链式操作结果。
type ChainResult struct {
	ChainID string         `json:"chain_id"`
	Phase   string         `json:"phase"`
	Status  string         `json:"status"`
	Error   string         `json:"error,omitempty"`
	raw     ChainRunner    `json:"-"`               // 原始 runner
	Extra   map[string]any `json:"extra,omitempty"` // 额外元数据
}

// AsCloudDownloadChain 返回原始 runner 的 CloudDownloadChain 引用。
// 如果原始 runner 不是 CloudDownloadChain 类型，返回 nil。
func (r *ChainResult) AsCloudDownloadChain() *CloudDownloadChain {
	if cdc, ok := r.raw.(*CloudDownloadChain); ok {
		return cdc
	}
	return nil
}

// LocalPath 获取本地路径（仅 CloudDownloadChain 支持）。
func (r *ChainResult) LocalPath() string {
	if r.Extra != nil {
		if v, ok := r.Extra["local_path"].(string); ok {
			return v
		}
	}
	return ""
}

// KeepFiles 返回是否保留远端文件（仅 CloudDownloadChain 支持）。
func (r *ChainResult) KeepFiles() bool {
	if r.Extra != nil {
		if v, ok := r.Extra["keep_files"].(bool); ok {
			return v
		}
	}
	return false
}

// chainOptions 链式操作选项。
type chainOptions struct {
	pollInterval time.Duration
	timeout      time.Duration
	keepFiles    bool
	progressFn   ProgressFunc
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

func WithChainProgress(fn ProgressFunc) ChainOption {
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
	store      KVStore
	codec      StructCodec
	registry   map[string]func() ChainRunner
	registryMu sync.RWMutex
	logger     *slog.Logger
}

func NewChainManager(store KVStore) *ChainManager {
	m := &ChainManager{
		store:    store,
		codec:    StructCodec{},
		registry: make(map[string]func() ChainRunner),
		logger:   slog.Default(),
	}
	// 从全局注册表拷贝默认值
	runnerRegistryMu.RLock()
	maps.Copy(m.registry, runnerRegistry)
	runnerRegistryMu.RUnlock()
	return m
}

// RegisterRunner 注册 runner 类型到实例注册表。
func (m *ChainManager) RegisterRunner(typeName string, factory func() ChainRunner) {
	m.registryMu.Lock()
	defer m.registryMu.Unlock()
	m.registry[typeName] = factory
}

// Run 执行链式操作（自动持久化，支持恢复）。
func (m *ChainManager) Run(ctx context.Context, runner ChainRunner) error {
	return m.RunWithProgress(ctx, runner, nil)
}

// RunWithProgress 执行链式操作，并支持外部进度回调。
func (m *ChainManager) RunWithProgress(ctx context.Context, runner ChainRunner, progressFn ProgressFunc) error {
	m.saveState(ctx, runner)
	// 注入 chainMgr 引用，使 runner 在阶段切换时能自行持久化状态
	runner.SetChainManager(m)
	reportFn := func(ctx context.Context, info ProgressInfo) {
		m.saveState(context.WithoutCancel(ctx), runner)
		if progressFn != nil {
			progressFn(ctx, info)
		}
	}
	err := runner.Run(ctx, reportFn)
	if err != nil {
		state := runner.State()
		state["status"] = StatusFailed
		if saveErr := m.store.Save(ctx, "chain:"+runner.ID(), state); saveErr != nil {
			return fmt.Errorf("链操作失败 (%w)，保存状态也失败: %v", err, saveErr)
		}
		return err
	}
	if delErr := m.store.Delete(ctx, "chain:"+runner.ID()); delErr != nil {
		return fmt.Errorf("链操作成功但清理状态失败: %w", delErr)
	}
	return nil
}

func (m *ChainManager) Resume(ctx context.Context, chainID string) (ChainRunner, error) {
	state, err := m.store.Load(ctx, "chain:"+chainID)
	if err != nil {
		return nil, fmt.Errorf("加载链状态失败: %w", err)
	}
	runner, err := m.resolveRunner(ctx, state)
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
			m.logger.DebugContext(ctx, "加载链状态失败", "key", key, "error", err)
			continue
		}
		status, _ := state["status"].(string)
		if status == StatusCompleted || status == StatusFailed {
			continue
		}
		runner, err := m.resolveRunner(ctx, state)
		if err != nil {
			m.logger.DebugContext(ctx, "解析链 runner 失败", "key", key, "error", err)
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
	if err := m.store.Save(ctx, "chain:"+runner.ID(), state); err != nil {
		m.logger.Warn("保存链状态失败", "chain_id", runner.ID(), "error", err)
	}
}

// resolveRunner 使用实例注册表解析 runner 类型。
// 先查实例注册表，未找到时回退到全局注册表。
func (m *ChainManager) resolveRunner(ctx context.Context, state map[string]any) (ChainRunner, error) {
	typeName, ok := state["type"].(string)
	if !ok {
		return nil, fmt.Errorf("state 缺少 type 字段")
	}
	// 先查实例注册表
	m.registryMu.RLock()
	factory, ok := m.registry[typeName]
	m.registryMu.RUnlock()
	if ok {
		return factory(), nil
	}
	// 未找到时回退到全局注册表
	runnerRegistryMu.RLock()
	factory, ok = runnerRegistry[typeName]
	runnerRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("未知的 runner 类型: %s", typeName)
	}
	return factory(), nil
}

// ResetRunners 清空实例注册表，用于测试隔离。
// 注意：仅清除实例注册表，不影响全局注册表。
func (m *ChainManager) ResetRunners() {
	m.registryMu.Lock()
	defer m.registryMu.Unlock()
	m.registry = make(map[string]func() ChainRunner)
}

// runnerRegistry 是 ChainRunner 类型全局注册表。
var (
	runnerRegistryMu sync.RWMutex
	runnerRegistry   = map[string]func() ChainRunner{}
)

func RegisterRunner(typeName string, factory func() ChainRunner) {
	runnerRegistryMu.Lock()
	defer runnerRegistryMu.Unlock()
	runnerRegistry[typeName] = factory
}

func UnregisterRunner(typeName string) {
	runnerRegistryMu.Lock()
	defer runnerRegistryMu.Unlock()
	delete(runnerRegistry, typeName)
}
