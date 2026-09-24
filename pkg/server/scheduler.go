// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// scheduler.go 是**统一任务调度器**（roadmap 11.10-H2，设计文档
// docs/designs/2026-09-24-task-scheduler.md）：
//   - 注册周期任务（唯一名 + 间隔 > 0 校验），Start 为每个任务起一个 goroutine
//     （ticker + stopCh，与既有 versionGCLoop/trashGCLoop 同构）；
//   - 维护窗口：MaintenanceOnly 任务在窗口外跳过（未配窗口 = 恒执行，零回归）；
//   - 单飞防重入：长任务阻塞期间多余 tick 被 CAS 挡下并 Warn；
//   - panic 恢复：任务 panic 记 Error 日志，循环存活（下个 tick 照常）；
//   - 优雅关闭：Stop 关 stopCh → wg.Wait；FinalRunOnStop 任务补跑一次
//     （share「退出前清理」语义）。
//
// 迁移对象：cleanupUploadingFilesLoop / versionGCLoop / trashGCLoop / ShareStore.cleanupLoop。
// 间隔/行为默认值 1:1 保留（见 routes.go 装配），零回归。

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// TaskFunc 任务执行体（ctx 供未来超时/取消扩展；本期恒 Background）。
type TaskFunc func(ctx context.Context)

// Task 是注册到 Scheduler 的周期任务描述。
type Task struct {
	// Name 是唯一任务名（重名 Register 报错）。
	Name string
	// Interval 是周期（必须 > 0，防 0 间隔忙循环）。
	Interval time.Duration
	// Run 是任务执行体（必须非 nil）。
	Run TaskFunc
	// MaintenanceOnly 为 true 时仅在维护窗口内执行（未配窗口 = 恒执行）。
	MaintenanceOnly bool
	// FinalRunOnStop 为 true 时 Stop 补跑一次（share「退出前清理」语义）。
	FinalRunOnStop bool
}

// taskEntry 是已注册任务的运行时状态。
type taskEntry struct {
	t       Task
	running atomic.Bool // 单飞防重入：true = 上次运行未结束，跳过本 tick
}

// Scheduler 是统一周期任务调度器。
type Scheduler struct {
	mu     sync.Mutex
	tasks  map[string]*taskEntry
	logger *slog.Logger

	stopCh    chan struct{}
	stopOnce  sync.Once
	finalOnce sync.Once
	startOnce sync.Once
	wg        sync.WaitGroup

	// inWindow 是维护窗口判定（nil = 关闭窗口，恒执行）。SetMaintenanceWindow 设置。
	inWindow func(time.Time) bool
}

// NewScheduler 创建调度器（logger nil 时用 slog.Default 兜底）。
func NewScheduler(logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		tasks:  make(map[string]*taskEntry),
		logger: logger,
		stopCh: make(chan struct{}),
	}
}

// Register 注册一个周期任务。校验 Name 非空、Interval > 0、Run 非 nil、不重名。
func (s *Scheduler) Register(t Task) error {
	if t.Name == "" {
		return errors.New("scheduler: 任务名不能为空")
	}
	if t.Interval <= 0 {
		return fmt.Errorf("scheduler: 任务 %q 间隔必须 > 0（got %v）", t.Name, t.Interval)
	}
	if t.Run == nil {
		return fmt.Errorf("scheduler: 任务 %q 缺 Run 执行体", t.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.tasks[t.Name]; dup {
		return fmt.Errorf("scheduler: 任务 %q 重复注册", t.Name)
	}
	s.tasks[t.Name] = &taskEntry{t: t}
	return nil
}

// SetMaintenanceWindow 设置维护窗口判定函数（nil = 关闭窗口，恒执行，默认零回归）。
// 可在运行中重复调用（测试窗口切换；装配层在 Start 前设置一次）。
func (s *Scheduler) SetMaintenanceWindow(in func(time.Time) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inWindow = in
}

// windowEnabled 返回当前维护窗口判定函数（nil = 恒执行）。
func (s *Scheduler) windowEnabled() func(time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inWindow
}

// Start 为全部已注册任务起 goroutine。幂等（sync.Once）：
// Start 之后再 Register 的任务不启动（须先注册后 Start）。
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		s.mu.Lock()
		entries := make([]*taskEntry, 0, len(s.tasks))
		for _, e := range s.tasks {
			entries = append(entries, e)
		}
		s.mu.Unlock()
		for _, e := range entries {
			s.wg.Add(1)
			go s.runLoop(e)
		}
	})
}

// Stop 停止全部任务 goroutine（关 stopCh → wg.Wait），FinalRunOnStop 任务补跑一次。
// 幂等（closeOnce + finalOnce）：重复调用安全。
// 无超时：任务挂死阻塞停服——与现状逐循环 wg.Wait 语义一致（设计文档明示）。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
	s.finalOnce.Do(func() {
		s.mu.Lock()
		entries := make([]*taskEntry, 0, len(s.tasks))
		for _, e := range s.tasks {
			entries = append(entries, e)
		}
		s.mu.Unlock()
		for _, e := range entries {
			if e.t.FinalRunOnStop {
				s.runOnce(e)
			}
		}
	})
}

// runLoop 是每个任务的 goroutine 形状（与既有循环同构：ticker + stopCh）：
// 窗口判断在 t.C 分支内 continue（下 tick 再查）。
func (s *Scheduler) runLoop(e *taskEntry) {
	defer s.wg.Done()
	ticker := time.NewTicker(e.t.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			if e.t.MaintenanceOnly {
				if in := s.windowEnabled(); in != nil && !in(now) {
					continue // 窗口外跳过，下 tick 再查
				}
			}
			s.runOnce(e)
		}
	}
}

// runOnce 执行单次任务：单飞 CAS 防重入 + panic 恢复（循环存活）。
func (s *Scheduler) runOnce(e *taskEntry) {
	if !e.running.CompareAndSwap(false, true) {
		s.logger.Warn("scheduler task skip: 上次运行未结束", "task", e.t.Name)
		return
	}
	defer e.running.Store(false)
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("scheduler task panic", "task", e.t.Name, "panic", r)
		}
	}()
	e.t.Run(context.Background())
}
