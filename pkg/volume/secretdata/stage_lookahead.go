// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"sync"
	"time"
)

// stage_lookahead.go —— **动态前瞻调度器**（2026-10-25 用户设计）。
//
// 设计（用户裁定）：
//   - 维护「读进度水位」与「未下载窗口数 ≥ lookAhead」的不变式；
//   - lookAhead 初始 = 1；消费快（客户端 1s > 1 窗）⇒ 增加；慢 ⇒ 回落；
//   - 每触发一个窗口下载，顺带把「读水位 + lookAhead」范围内**未下载**的窗口入队
//     （维持未下载窗口数 ≥ lookAhead ⇒ 读取永远有预取缓冲）；
//   - 带宽感知：窗口 4MiB @ 带宽 8MB/s = 0.5s/窗；客户端若 1s 消费 2 窗 ⇒ lookAhead
//     增至 2，预取维持 2 窗领先。
//
// 实现：读路径在每次窗口读取后调用 `noteRead(windowIdx)` 推进水位；调度器按水位+
// lookAhead 把未下载窗口交给下载队列。下载完成回调 `noteReady` 更新未下载计数。

// lookAheadConfig 是前瞻调度的参数。
type lookAheadConfig struct {
	// MinLookAhead 是初始/最小未下载窗口数（=1）。
	MinLookAhead int
	// MaxLookAhead 是最大前瞻窗口数（带宽上限：窗口 4MiB × Max ≤ 网盘突发缓冲）。
	MaxLookAhead int
	// WindowBytes 是单窗字节（用于速率计算）。
	WindowBytes int64
}

// defaultLookAheadConfig 返回默认前瞻参数（窗口 4MiB；1..8 窗）。
func defaultLookAheadConfig() lookAheadConfig {
	return lookAheadConfig{
		MinLookAhead: 1,
		MaxLookAhead: 16,
		WindowBytes:  stageWindowTargetBytes,
	}
}

// lookAheadTracker 是动态前瞻的状态机（并发安全；纯内存，无需持久化）。
type lookAheadTracker struct {
	cfg lookAheadConfig

	mu sync.Mutex
	// readWatermark 是已读窗口序号（0-based，按 planCacheWindows 输出顺序）。
	readWatermark int
	// inflightIdx 记录正在下载的窗口序号（防重复入队）。
	inflightIdx map[int]struct{}
	// totalWindows 是当前文件的窗口总数（planFor 后更新）。
	totalWindows int
	// lookAhead 是动态前瞻值（初始 MinLookAhead）。
	lookAhead int
	// lastMove time 用于速率估计（消费 1 窗的时间）。
	lastRead time.Time
	// rateEma 是消费速率的指数平均（窗/秒）。
	rateEma float64
}

// newLookAheadTracker 构造前瞻跟踪器。
func newLookAheadTracker(cfg lookAheadConfig) *lookAheadTracker {
	if cfg.MinLookAhead <= 0 {
		cfg.MinLookAhead = 1
	}
	if cfg.MaxLookAhead < cfg.MinLookAhead {
		cfg.MaxLookAhead = cfg.MinLookAhead
	}
	return &lookAheadTracker{
		cfg:         cfg,
		inflightIdx: map[int]struct{}{},
		lookAhead:   cfg.MinLookAhead,
		lastRead:    time.Now(),
	}
}

// noteRead 记录一次窗口消费（客户端读了一个窗口）：推进水位 + 更新消费速率 + 动态前瞻。
// 返回「本次读后应入队的未下载窗口数」（供调用方调度下载）。
func (t *lookAheadTracker) noteRead(windowIdx int) []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if windowIdx > t.readWatermark {
		// 推进水位（按顺序读；跳读也把水位推到 max）。
		t.readWatermark = windowIdx
	}
	// 消费速率 EMA（窗/秒）：距上次读 > 0 才更新。
	if dt := now.Sub(t.lastRead); dt > 0 && dt < 10*time.Second {
		rate := 1.0 / dt.Seconds()
		if t.rateEma == 0 {
			t.rateEma = rate
		} else {
			t.rateEma = 0.7*t.rateEma + 0.3*rate
		}
		t.lastRead = now
	}
	// 动态前瞻：消费快（rate > 1/窗秒）⇒ 增加；慢 ⇒ 回落（不低于 Min）。
	want := t.lookAhead
	if t.rateEma > 1.0 {
		want = int(t.rateEma) + 1
	}
	if want > t.cfg.MaxLookAhead {
		want = t.cfg.MaxLookAhead
	}
	if want < t.cfg.MinLookAhead {
		want = t.cfg.MinLookAhead
	}
	t.lookAhead = want
	return t.toEnqueueLocked()
}

// toEnqueueLocked 返回「未下载窗口数 < lookAhead」时需补入队的窗口序号（升序）。
func (t *lookAheadTracker) toEnqueueLocked() []int {
	target := t.readWatermark + t.lookAhead
	var out []int
	for idx := t.readWatermark + 1; idx <= target && idx < t.totalWindows; idx++ {
		if _, running := t.inflightIdx[idx]; running {
			continue
		}
		out = append(out, idx)
	}
	return out
}

// setTotalWindows 设置文件窗口总数（planFor 后）。
func (t *lookAheadTracker) setTotalWindows(n int) {
	t.mu.Lock()
	t.totalWindows = n
	t.mu.Unlock()
}
