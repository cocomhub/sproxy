// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cluster_index_sync.go 是集群索引一致性装配层（roadmap 11.11 方案 A-④）：
//   - IndexSyncLoop：订阅 StateStore.Watch("index/") → Load → ReloadIndex（rev 幂等）；
//     通道关闭退避重连（1s→2s→4s→封顶 10s）；损坏回退 InvalidateIndex 全量重建。
//   - ResyncLoop：周期 List("index/") 比对 → 落后重载（覆盖事件/Watch 丢失窗口）。
//
// 未装配（StateStore nil）→ 全部 no-op，单节点零回归。

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/state"
)

// indexSyncAdapter 把 WatchStore 适配 files.IndexSync（主节点 Publish/Load）。
type indexSyncAdapter struct {
	st     state.StateStore
	logger *slog.Logger
	prefix string
}

// Publish 序列化信封 → StateStore.Put(index/<owner>, envelope)。
func (a *indexSyncAdapter) Publish(ctx context.Context, owner string, entries map[string]*files.IndexSnapshotEntry) error {
	env := &files.IndexEnvelope{
		Rev:     time.Now().UnixNano(), // 主节点 rev：时间戳单调（简化；正式按 per-owner 计数）
		Node:    "local",
		Updated: time.Now().UnixNano(),
		Entries: entries,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return a.st.Put(ctx, a.prefix+owner, data)
}

// Load 从 StateStore Get 读信封。
func (a *indexSyncAdapter) Load(ctx context.Context, owner string) (*files.IndexEnvelope, error) {
	data, err := a.st.Get(ctx, a.prefix+owner)
	if err != nil {
		return nil, err
	}
	var env files.IndexEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// indexSyncLoop 是副本 Watch 循环（订阅 index/ 前缀变更 → ReloadIndex）。
type indexSyncLoop struct {
	ctx    context.Context
	st     state.WatchStore
	svc    indexSyncTarget
	prefix string
	logger *slog.Logger
	stop   chan struct{}
	wg     sync.WaitGroup
	ready  chan struct{} // Start 后订阅成功即关闭（测试同步）
}

// indexSyncTarget 是装配层需要的 files.Service 窄接口（测试探针注入）。
type indexSyncTarget interface {
	ReloadIndex(owner string, env *files.IndexEnvelope) bool
	InvalidateIndex(owner string)
}

// newIndexSyncLoop 构造 Watch 循环（ctx 取消停止）。
func newIndexSyncLoop(ctx context.Context, st state.WatchStore, svc indexSyncTarget, prefix string, logger *slog.Logger) *indexSyncLoop {
	l := &indexSyncLoop{ctx: ctx, st: st, svc: svc, prefix: prefix, logger: logger, stop: make(chan struct{})}
	l.ready = make(chan struct{})
	return l
}

// Start 启动 Watch 循环（后台 goroutine；阻塞至首次订阅成功，测试同步用）。
func (l *indexSyncLoop) Start() {
	l.wg.Add(1)
	go l.run()
	select {
	case <-l.ready:
	case <-l.ctx.Done():
	}
}

// run 订阅 Watch 并消费变更（通道关闭退避重连 1s→10s 封顶）。
func (l *indexSyncLoop) run() {
	defer l.wg.Done()
	backoff := time.Second
	for {
		ch, err := l.st.Watch(l.ctx, l.prefix)
		if err != nil {
			l.logger.Warn("Watch 建立失败", "error", err)
			select {
			case <-l.ctx.Done():
				return
			case <-time.After(backoff):
				backoff = minDuration(backoff*2, 10*time.Second)
				continue
			}
		}
		backoff = time.Second // 连接成功重置退避
		select {
		case <-l.ready: // 已关闭（幂等）
		default:
			close(l.ready)
		}
		l.consume(ch)
		select {
		case <-l.ctx.Done():
			return
		case <-l.stop:
			return
		default:
		}
	}
}

// consume 消费变更通道。
func (l *indexSyncLoop) consume(ch <-chan state.Change) {
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-l.stop:
			return
		case c, ok := <-ch:
			if !ok {
				return // 通道关闭 → 退避重连
			}
			if c.Op != "put" {
				continue
			}
			owner := strings.TrimPrefix(c.Key, l.prefix)
			if owner == "" || owner == c.Key {
				continue
			}
			l.handleChange(owner)
		}
	}
}

// handleChange 载入信封 → ReloadIndex（失败回退 InvalidateIndex）。
func (l *indexSyncLoop) handleChange(owner string) {
	l.logger.Debug("索引 Change 到达", "owner", owner)
	ss, ok := l.st.(state.StateStore)
	if !ok {
		l.logger.Warn("WatchStore 不实现 StateStore（装配错）", "owner", owner)
		return
	}
	adapter := &indexSyncAdapter{st: ss, logger: l.logger, prefix: l.prefix}
	env, err := adapter.Load(l.ctx, owner)
	if err != nil {
		l.logger.Warn("索引信封载入失败", "owner", owner, "error", err)
		l.svc.InvalidateIndex(owner) // fail-safe：全量重建
		return
	}
	if !l.svc.ReloadIndex(owner, env) {
		// 旧 rev / 损坏 → 保持现状（旧 rev 幂等忽略）；损坏已由 ReloadIndex false 表达
		l.logger.Debug("索引信封未应用", "owner", owner, "rev", env.Rev)
	}
}

// Stop 停止循环。
func (l *indexSyncLoop) Stop() {
	close(l.stop)
	l.wg.Wait()
}

// minDuration 返回较小值。
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// resyncLoop 是周期兜底：List("index/") 比对各 owner 快照 → 落后重载。
// 简化实现：周期全量 List → 逐个 Load → ReloadIndex（rev 幂等天然去重）。
type resyncLoop struct {
	ctx      context.Context
	st       state.StateStore
	svc      indexSyncTarget
	prefix   string
	interval time.Duration
	logger   *slog.Logger
	stop     chan struct{}
	wg       sync.WaitGroup
}

// newResyncLoop 构造 resync 循环。
func newResyncLoop(ctx context.Context, st state.StateStore, svc indexSyncTarget, prefix string, interval time.Duration, logger *slog.Logger) *resyncLoop {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &resyncLoop{ctx: ctx, st: st, svc: svc, prefix: prefix, interval: interval, logger: logger, stop: make(chan struct{})}
}

// Start 启动周期 resync。
func (r *resyncLoop) Start() {
	r.wg.Add(1)
	go r.run()
}

// run 周期 List → Load → ReloadIndex。
func (r *resyncLoop) run() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.stop:
			return
		case <-ticker.C:
			r.resyncOnce()
		}
	}
}

// resyncOnce 执行一轮全量比对。
func (r *resyncLoop) resyncOnce() {
	keys, err := r.st.List(r.ctx, r.prefix)
	if err != nil {
		r.logger.Warn("resync List 失败", "error", err)
		return
	}
	for _, k := range keys {
		owner := strings.TrimPrefix(k, r.prefix)
		if owner == "" || owner == k {
			continue
		}
		adapter := &indexSyncAdapter{st: r.st, logger: r.logger, prefix: r.prefix}
		env, err := adapter.Load(r.ctx, owner)
		if err != nil {
			continue
		}
		if !r.svc.ReloadIndex(owner, env) {
			r.svc.InvalidateIndex(owner)
		}
	}
}

// Stop 停止 resync。
func (r *resyncLoop) Stop() {
	close(r.stop)
	r.wg.Wait()
}
