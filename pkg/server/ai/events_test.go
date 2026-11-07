// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// events_test.go 验证 AI 事件消费端（AIEventConsumer）：
//  1. Start 周期拉取 EventSource 快照 → worker 调 enqueue（Start 幂等）；
//  2. 未装配（bus/enqueue 为 nil）→ Start/Stop no-op；
//  3. HandleEvent：非文件动作忽略、rev 幂等、去重窗口（含过期后重新入队）、op 原样透传；
//  4. 有界队列满 → 丢弃不阻塞；enqueue panic → 恢复不扩散；
//  5. 默认队列容量/去重窗口。

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
)

// fakeEventSource 按批次返回预置事件（批次耗尽后返回 nil，覆盖空快照分支）。
type fakeEventSource struct {
	mu      sync.Mutex
	batches [][]FileEvent
}

func (s *fakeEventSource) DrainFileEvents() []FileEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return nil
	}
	batch := s.batches[0]
	s.batches = s.batches[1:]
	return batch
}

// waitTask 等待任务入列（超时即失败，不使用固定 sleep）。
func waitTask(t *testing.T, ch <-chan Task) Task {
	t.Helper()
	select {
	case task := <-ch:
		return task
	case <-time.After(3 * time.Second):
		t.Fatal("等待 AI 流水线任务超时")
		return Task{}
	}
}

// TestAIEventConsumer_StartDrainsEvents Start 周期拉取 → worker 调 enqueue；重复 Start 幂等。
func TestAIEventConsumer_StartDrainsEvents(t *testing.T) {
	t.Parallel()
	src := &fakeEventSource{batches: [][]FileEvent{{
		{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: 1},
	}}}
	tasks := make(chan Task, 4)
	c := NewAIEventConsumer(src, func(owner, rel, op string) {
		tasks <- Task{Owner: owner, Rel: rel, Op: op}
	}, config.AIEventsConfig{}, nil)
	c.Start()
	c.Start() // once.Do 幂等：不得启动第二对 goroutine
	t.Cleanup(c.Stop)

	task := waitTask(t, tasks)
	if task.Owner != "alice" || task.Rel != "a.txt" || task.Op != "upload" {
		t.Fatalf("任务载荷不符: %+v", task)
	}
	c.Stop()
	c.Stop() // 重复 Stop 幂等
}

// TestAIEventConsumer_StartNoopWhenUnwired bus 或 enqueue 为 nil → Start no-op；Stop 可重复。
func TestAIEventConsumer_StartNoopWhenUnwired(t *testing.T) {
	t.Parallel()
	called := make(chan struct{}, 1)
	noBus := NewAIEventConsumer(nil, func(string, string, string) { called <- struct{}{} }, config.AIEventsConfig{}, nil)
	noBus.Start()
	noBus.Stop()

	src := &fakeEventSource{batches: [][]FileEvent{{
		{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: 1},
	}}}
	noEnqueue := NewAIEventConsumer(src, nil, config.AIEventsConfig{}, nil)
	noEnqueue.Start()
	noEnqueue.Stop()

	select {
	case <-called:
		t.Fatal("未装配 bus 时不应有任何回调")
	case <-time.After(300 * time.Millisecond):
	}

	var nilConsumer *AIEventConsumer
	nilConsumer.Stop() // nil 接收者 no-op
}

// TestNewAIEventConsumer_Defaults 队列容量/去重窗口缺省值。
func TestNewAIEventConsumer_Defaults(t *testing.T) {
	t.Parallel()
	c := NewAIEventConsumer(nil, func(string, string, string) {}, config.AIEventsConfig{}, nil)
	if got := cap(c.queue); got != 256 {
		t.Fatalf("默认队列容量应为 256，got %d", got)
	}
	if c.dedupWindow != 5*time.Minute {
		t.Fatalf("默认去重窗口应为 5m，got %v", c.dedupWindow)
	}

	custom := NewAIEventConsumer(nil, func(string, string, string) {}, config.AIEventsConfig{
		QueueSize:   2,
		DedupWindow: time.Minute,
	}, nil)
	if got := cap(custom.queue); got != 2 {
		t.Fatalf("自定义队列容量应为 2，got %d", got)
	}
	if custom.dedupWindow != time.Minute {
		t.Fatalf("自定义去重窗口应为 1m，got %v", custom.dedupWindow)
	}
}

// TestAIEventConsumer_HandleEventRoutesFileActions 文件动作原样透传为 op。
func TestAIEventConsumer_HandleEventRoutesFileActions(t *testing.T) {
	t.Parallel()
	actions := []string{"upload", "delete", "rename", "mkdir", "rmdir", "version", "restore", "file.chmod"}
	c := NewAIEventConsumer(nil, func(string, string, string) {}, config.AIEventsConfig{}, nil)
	for i, action := range actions {
		c.HandleEvent(FileEvent{
			Action: action,
			Owner:  "alice",
			Rel:    fmt.Sprintf("f%d.txt", i), // 不同 rel → 不被去重窗口合并
			Cursor: uint64(i + 1),
		})
	}
	for i, action := range actions {
		task := waitTask(t, c.queue)
		if task.Op != action {
			t.Fatalf("op 应原样透传 %q，got %q", action, task.Op)
		}
		if task.Owner != "alice" || task.Rel != fmt.Sprintf("f%d.txt", i) {
			t.Fatalf("任务载荷不符: %+v", task)
		}
	}
}

// TestAIEventConsumer_HandleEventIgnoresNonFileAction 非文件动作不入队、不建去重状态。
func TestAIEventConsumer_HandleEventIgnoresNonFileAction(t *testing.T) {
	t.Parallel()
	c := NewAIEventConsumer(nil, func(string, string, string) {}, config.AIEventsConfig{}, nil)
	c.HandleEvent(FileEvent{Action: "share.create", Owner: "alice", Rel: "a.txt", Cursor: 1})
	if got := len(c.queue); got != 0 {
		t.Fatalf("非文件动作不应入队，queue=%d", got)
	}
	c.mu.Lock()
	_, hasRev := c.lastRev["alice\x00a.txt"]
	c.mu.Unlock()
	if hasRev {
		t.Fatal("非文件动作不应写入游标")
	}
}

// TestAIEventConsumer_RevIdempotent 旧游标事件跳过；新游标刷新游标。
func TestAIEventConsumer_RevIdempotent(t *testing.T) {
	t.Parallel()
	c := NewAIEventConsumer(nil, func(string, string, string) {}, config.AIEventsConfig{}, nil)
	c.HandleEvent(FileEvent{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: 10})
	waitTask(t, c.queue)

	c.HandleEvent(FileEvent{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: 5})
	c.HandleEvent(FileEvent{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: 10})
	if got := len(c.queue); got != 0 {
		t.Fatalf("旧/等值游标应跳过，queue=%d", got)
	}
	c.mu.Lock()
	rev := c.lastRev["alice\x00a.txt"]
	c.mu.Unlock()
	if rev != 10 {
		t.Fatalf("游标不应倒退，got %d", rev)
	}
}

// TestAIEventConsumer_DedupWindow 窗口内同 key 只入队一次并刷新游标；窗口过期后重新入队。
func TestAIEventConsumer_DedupWindow(t *testing.T) {
	t.Parallel()
	window := time.Hour
	c := NewAIEventConsumer(nil, func(string, string, string) {}, config.AIEventsConfig{DedupWindow: window}, nil)

	for cursor := uint64(1); cursor <= 3; cursor++ {
		c.HandleEvent(FileEvent{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: cursor})
	}
	task := waitTask(t, c.queue)
	if task.Op != "upload" {
		t.Fatalf("首个事件应入队: %+v", task)
	}
	if got := len(c.queue); got != 0 {
		t.Fatalf("窗口内后续事件应被去重，queue=%d", got)
	}
	c.mu.Lock()
	rev := c.lastRev["alice\x00a.txt"]
	c.mu.Unlock()
	if rev != 3 {
		t.Fatalf("去重时应刷新游标到 3，got %d", rev)
	}

	// 窗口过期：把上次入队时间挪到窗口之外 → 新事件重新入队。
	c.mu.Lock()
	c.dedup["alice\x00a.txt"] = time.Now().Add(-2 * window)
	c.mu.Unlock()
	c.HandleEvent(FileEvent{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: 4})
	task = waitTask(t, c.queue)
	if task.Op != "upload" {
		t.Fatalf("窗口过期后应重新入队: %+v", task)
	}

	// 不同 rel 不受同 key 去重影响。
	c.HandleEvent(FileEvent{Action: "delete", Owner: "alice", Rel: "b.txt", Cursor: 5})
	if task := waitTask(t, c.queue); task.Rel != "b.txt" || task.Op != "delete" {
		t.Fatalf("不同 rel 应独立入队: %+v", task)
	}
}

// TestAIEventConsumer_QueueFullDrops 队列满 → 丢弃（不阻塞、不 panic，周期扫描兜底）。
func TestAIEventConsumer_QueueFullDrops(t *testing.T) {
	t.Parallel()
	c := NewAIEventConsumer(nil, func(string, string, string) {}, config.AIEventsConfig{QueueSize: 1}, nil)
	c.HandleEvent(FileEvent{Action: "upload", Owner: "alice", Rel: "a.txt", Cursor: 1})
	c.HandleEvent(FileEvent{Action: "upload", Owner: "alice", Rel: "b.txt", Cursor: 2})
	if got := len(c.queue); got != 1 {
		t.Fatalf("队列满应只保留 1 条，got %d", got)
	}
	if task := waitTask(t, c.queue); task.Rel != "a.txt" {
		t.Fatalf("保留的应是首条任务: %+v", task)
	}
}

// TestAIEventConsumer_SafeEnqueueRecoversPanic 回调 panic 被恢复（消费者不扩散崩溃）。
func TestAIEventConsumer_SafeEnqueueRecoversPanic(t *testing.T) {
	t.Parallel()
	c := NewAIEventConsumer(nil, func(string, string, string) { panic("boom") }, config.AIEventsConfig{}, testAILogger())
	c.safeEnqueue(Task{Owner: "alice", Rel: "a.txt", Op: "upload"}) // 不 panic 即通过
}
