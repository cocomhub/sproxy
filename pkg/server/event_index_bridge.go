// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// event_index_bridge.go 是集群写面协调（roadmap 11.11 方案 A-⑥ W3）：
// EventBus（文件变更事件）→ 副本索引失效桥接。事件是「低延迟失效信号」——
// 收到 file.* 事件 → 立即 InvalidateIndex(owner)（副本重载走 index-consistency 的
// ReloadIndex/全量重建入口）；一致性事实源仍在 StateStore 快照（Watch/resync 兜底）。
//
// 零回归：未装配（bridge nil）→ 事件仅广播 SSE，无额外失效动作。

import (
	"log/slog"
	"strings"
	"sync"
	"time"
)

// indexInvalidator 是副本失效目标（files.Service 窄接口 + 测试探针）。
type indexInvalidator interface {
	InvalidateIndex(owner string)
}

// eventIndexBridge 订阅 EventBus 文件事件 → 副本索引失效。
type eventIndexBridge struct {
	bus    *EventBus
	svc    indexInvalidator
	logger *slog.Logger
	stop   chan struct{}
	wg     sync.WaitGroup
}

// NewEventIndexBridge 构造桥接（装配层导出）。
func NewEventIndexBridge(bus *EventBus, svc indexInvalidator, logger *slog.Logger) *eventIndexBridge {
	return newEventIndexBridge(bus, svc, logger)
}

// newEventIndexBridge 构造桥接（订阅 file 动作前缀）。
func newEventIndexBridge(bus *EventBus, svc indexInvalidator, logger *slog.Logger) *eventIndexBridge {
	return &eventIndexBridge{bus: bus, svc: svc, logger: logger, stop: make(chan struct{})}
}

// Start 启动桥接（后台 goroutine 消费事件）。
func (b *eventIndexBridge) Start() {
	b.wg.Add(1)
	go b.run()
}

// Stop 停止桥接。
func (b *eventIndexBridge) Stop() {
	close(b.stop)
	b.wg.Wait()
}

// run 周期扫描 EventBus 各 owner ring 的未消费事件 → 失效。
// 简化实现：EventBus 是推送式（订阅 chan），此处用轮询 ring 快照驱动——
// 更稳：直接复用 EventBus.Subscribe（每 owner 订阅成本高）；改用全局扫描：
// EventBus 新增 DrainFileEvents 供桥接拉取（见下方）。
func (b *eventIndexBridge) run() {
	defer b.wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.drainOnce()
		}
	}
}

// drainOnce 拉取全部未消费文件事件 → 失效对应 owner。
func (b *eventIndexBridge) drainOnce() {
	for _, ev := range b.bus.DrainFileEvents() {
		if !isFileAction(ev.Action) {
			continue
		}
		b.svc.InvalidateIndex(ev.Owner)
		b.logger.Debug("事件驱动索引失效", "owner", ev.Owner, "action", ev.Action, "rel", ev.Rel)
	}
}

// isFileAction 判断事件是否文件变更动作（upload/delete/rename/mkdir/rmdir/version 族）。
func isFileAction(action string) bool {
	switch action {
	case "upload", "delete", "rename", "mkdir", "rmdir", "version", "restore":
		return true
	}
	return strings.HasPrefix(action, "file.")
}
