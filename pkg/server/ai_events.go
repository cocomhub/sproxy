// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_events.go 是 AI 事件流水线（roadmap 12.2-4，设计文档
// docs/designs/2026-09-24-ai-event-pipeline.md）：AIEventConsumer 订阅 EventBus
// 文件变更事件 → 去重入队 → worker 调用 enqueue 回调（向量化/摘要/打标任务）。
//
// 消费语义（设计 §2.2）：
//   - 去重窗口：同 (owner, rel) 在 dedup_window 内只入队一次（防事件风暴）；
//   - rev 幂等：per-(owner,rel) 缓存已处理游标，旧事件直接跳过；
//   - delete/rmdir → 回调 op=delete（删除条目，不重新生成）；
//   - rename → 回调 op=rename（key 迁移）；
//   - 有界队列：queue 满 → 丢弃 + Warn（事件只是「及时触发」，正确性靠周期扫描兜底）。
//
// 事件源形态与 event_index_bridge 同构：EventBus 是 per-owner 推送式（Subscribe
// 按 owner 分 ring，消费端无法预知全部 owner），故用 DrainFileEvents 周期拉取
// 全部 owner ring 快照 + per-(owner,rel) 游标幂等去重（与桥接同一模式）。
//
// 零回归：ai.events.enabled=false → 不装配；EventBus 未装配 → Warn + 不启动
// （可观测降级，流水线只跑手动触发/周期扫描）。

import (
	"log/slog"
	"sync"
	"time"
)

// aiEventsConfig 是 ai.events 配置段（config.go 引用；默认关零回归）。
type aiEventsConfig struct {
	Enabled     bool          `yaml:"enabled" mapstructure:"enabled"`
	QueueSize   int           `yaml:"queue_size" mapstructure:"queue_size"`     // 默认 256
	DedupWindow time.Duration `yaml:"dedup_window" mapstructure:"dedup_window"` // 默认 5m
}

// AIEventsConfig 是 ai.events 配置段（导出供装配层）。
type AIEventsConfig = aiEventsConfig

// aiTask 是一条入队任务（owner + rel + op）。
type aiTask struct {
	owner string
	rel   string
	op    string
}

// AIEventConsumer 订阅 EventBus 文件事件并路由到 AI 流水线任务队列。
type AIEventConsumer struct {
	bus     *EventBus // nil = 未装配（Start 不启动，零回归）
	enqueue func(owner, rel, op string)
	logger  *slog.Logger

	// queue 是有界任务队列（满则丢弃 + Warn；正确性靠周期扫描兜底）。
	queue chan aiTask

	// dedup 是 (owner,rel) → 上次入队时间（窗口内去重，防事件风暴）。
	dedup map[string]time.Time
	// lastRev 是 per-(owner,rel) 已处理游标（rev 幂等：旧事件跳过）。
	lastRev map[string]uint64
	mu      sync.Mutex

	dedupWindow time.Duration

	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// NewAIEventConsumer 构造消费端（装配层导出；bus nil = 未装配零回归）。
func NewAIEventConsumer(bus *EventBus, enqueue func(owner, rel, op string), cfg AIEventsConfig, logger *slog.Logger) *AIEventConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = 256
	}
	window := cfg.DedupWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &AIEventConsumer{
		bus:         bus,
		enqueue:     enqueue,
		logger:      logger,
		queue:       make(chan aiTask, queueSize),
		dedup:       map[string]time.Time{},
		lastRev:     map[string]uint64{},
		dedupWindow: window,
		stop:        make(chan struct{}),
	}
}

// Start 启动消费端：周期拉取 EventBus 快照 + 后台 worker 处理队列。
// bus nil（未装配）→ 不启动（零回归）；重复 Start 幂等。
func (c *AIEventConsumer) Start() {
	if c == nil || c.bus == nil || c.enqueue == nil {
		return
	}
	c.once.Do(func() {
		c.wg.Add(1)
		go c.run()
		c.wg.Add(1)
		go c.worker()
	})
}

// Stop 停止消费端（等 goroutine 退出）。
func (c *AIEventConsumer) Stop() {
	if c == nil {
		return
	}
	select {
	case <-c.stop:
		return
	default:
		close(c.stop)
	}
	c.wg.Wait()
}

// drainInterval 是事件拉取周期（与 event_index_bridge 同量级：低延迟 + 低开销）。
const drainInterval = 100 * time.Millisecond

// run 周期拉取 EventBus 全部 owner ring 快照 → handleEvent。
func (c *AIEventConsumer) run() {
	defer c.wg.Done()
	ticker := time.NewTicker(drainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			for _, ev := range c.bus.DrainFileEvents() {
				c.handleEvent(ev)
			}
		}
	}
}

// worker 处理有界队列（调 enqueue 回调）。
func (c *AIEventConsumer) worker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.stop:
			return
		case t := <-c.queue:
			c.safeEnqueue(t)
		}
	}
}

// safeEnqueue 调 enqueue 回调并 recover（消费端 panic 不扩散，设计 §4）。
func (c *AIEventConsumer) safeEnqueue(t aiTask) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Warn("AI 流水线 enqueue 回调 panic 已恢复", "owner", t.owner, "rel", t.rel, "op", t.op, "panic", r)
		}
	}()
	c.enqueue(t.owner, t.rel, t.op)
}

// handleEvent 处理一条文件事件：去重 + rev 幂等 + 路由。
// 供测试直调（白盒）；生产由 run 经 DrainFileEvents 拉取后调用。
func (c *AIEventConsumer) handleEvent(ev fileEventSnapshot) {
	if !isFileAction(ev.Action) {
		return
	}
	key := ev.Owner + "\x00" + ev.Rel

	c.mu.Lock()
	// rev 幂等：旧游标跳过（EventBus 游标单调，等价事件序 rev）。
	if last, ok := c.lastRev[key]; ok && ev.Cursor <= last {
		c.mu.Unlock()
		return
	}
	// 去重窗口：同 key 在窗口内已入队 → 只刷新游标不入队。
	if t, ok := c.dedup[key]; ok && time.Since(t) < c.dedupWindow {
		c.lastRev[key] = ev.Cursor
		c.mu.Unlock()
		return
	}
	c.dedup[key] = time.Now()
	c.lastRev[key] = ev.Cursor
	c.mu.Unlock()

	// 路由：delete/rmdir/rename 原样透传 op（消费侧据此删条目/迁移 key）；
	// upload/version/restore → op=原动作（重新生成向量/摘要）。
	c.enqueueTask(aiTask{owner: ev.Owner, rel: ev.Rel, op: ev.Action})
}

// enqueueTask 入有界队列（满则丢弃 + Warn——事件只是及时触发，正确性靠周期扫描）。
func (c *AIEventConsumer) enqueueTask(t aiTask) {
	select {
	case c.queue <- t:
	default:
		c.logger.Warn("AI 流水线任务队列已满，丢弃事件（周期扫描兜底）", "owner", t.owner, "rel", t.rel, "op", t.op)
	}
}
