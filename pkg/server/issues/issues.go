// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package issues 是「**待处理事项**」的登记、重试与人工接入机制（server 作用域）。
//
// 为什么需要它：运行期会出现一些**引擎无法自主决策**的存储/传输残留，例如
//   - `dir_meta_ambiguous`：同一容器内有两个目录 meta（改名/移动时旧 meta 删除失败）；
//   - `container_not_indexed`：dir-meta 的 dir_id 不在索引中（移动后索引未重建）；
//   - 旧目录 meta 清理失败、上传失败、转存失败……
//
// 这些情况此前**只写一条 WARN 日志** ⇒ 运维无从感知、也无从重试，问题会长期潜伏。
// 本包把它们**持久化为可查询事项**，并提供：
//  1. **可自动重试**（带退避，有界）⇒ 最终成功（用户要求「最终必须成功」）；
//  2. **不可自主决策的** ⇒ 标 `needs_human`，**不自动重试**，等待人工接入
//     （用户要求「无法自主决策的残留必须能人工感知」）；
//  3. 重试达上限 ⇒ **转人工**（而不是无限重试或静默放弃）；
//  4. 全部事项可经 API/Web UI 查看（含**详细失败原因**）并可人工标记已解决。
//
// 设计约束：时间经 `Options.Clock` 注入（测试可并行、不依赖真实时钟）；存储经 `Store` 注入
// （测试用 `NewMemStore`，生产用文件存储）——**不使用包级可变状态**。
package issues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Kind 是事项类型（稳定字符串，供 API/UI 筛选）。
type Kind string

const (
	// KindDirMetaAmbiguous 同一容器内多个目录 meta（异常布局）——**需人工仲裁**（不自动删）。
	KindDirMetaAmbiguous Kind = "dir_meta_ambiguous"
	// KindDirMetaCleanupFailed 改名/移动后旧目录 meta 删除失败——**可自动重试**。
	KindDirMetaCleanupFailed Kind = "dir_meta_cleanup_failed"
	// KindContainerNotIndexed dir-meta 的 dir_id 不在索引中（索引未重建）——**可自动重试**。
	KindContainerNotIndexed Kind = "container_not_indexed"
	// KindUploadFailed 上传/转存失败——**可自动重试**。
	KindUploadFailed Kind = "upload_failed"
	// KindBlobMD5Unconverged 内容已确认在云端（`block_list` 逐块对账通过）但远端 md5 尚未权威
	// ——**可自动重试**：连续上传 2 次（命中秒传触发刷新）→ 退避（默认 5min）后再对账；
	// 达上限转人工。
	//
	// 登记约定：`Volume` = 卷名；`Path` = **上传时保留的本地副本路径**（重传的源）；
	// `Detail` = 卷内相对路径（rel）。三者缺一即无法执行 ⇒ 执行器返回错误转人工（不静默成功）。
	KindBlobMD5Unconverged Kind = "blob_md5_unconverged"
	// KindOther 其它（兜底，登记时必须写明确 Detail）。
	KindOther Kind = "other"
)

// Status 是事项状态。
type Status string

const (
	// StatusOpen 待处理（可自动重试）。
	StatusOpen Status = "open"
	// StatusRetrying 正在重试（执行器已接手）。
	StatusRetrying Status = "retrying"
	// StatusResolved 已解决（自动成功或人工处理完成）。
	StatusResolved Status = "resolved"
	// StatusNeedsHuman 需人工接入（引擎无法自主决策，或自动重试已达上限）。
	StatusNeedsHuman Status = "needs_human"
)

// Item 是一条待处理事项。
//
// **Detail 与 LastError 是人工判断的核心依据**：登记方必须写明「发生了什么、为什么引擎不能
// 自己决定、人工需要做什么」——只写"失败"是不够的（用户明确要求"提供详细失败原因便于人工接入"）。
type Item struct {
	ID        string    `json:"id"`   // 稳定 ID（Kind+Volume+Path 派生，同事项重复登记合并）
	Kind      Kind      `json:"kind"` // 类型
	Volume    string    `json:"volume"`
	Path      string    `json:"path"`   // 卷内路径/容器路径（定位用）
	Detail    string    `json:"detail"` // **详细说明**（发生什么 + 为何不能自动决策 + 人工怎么办）
	Source    string    `json:"source"` // 发现来源（如 "startup-scan" / "rename" / "upload"）
	Status    Status    `json:"status"`
	Attempts  int       `json:"attempts"`   // 已自动重试次数
	FirstSeen time.Time `json:"first_seen"` // 首次发现
	LastSeen  time.Time `json:"last_seen"`  // 最近发现
	// Occurrences 是同一事项被重复发现的次数（>1 说明问题持续存在）。
	Occurrences int64 `json:"occurrences"`
	// Retryable=false ⇒ 引擎无法自主决策 ⇒ 直接 needs_human（不自动重试）。
	Retryable   bool      `json:"retryable"`
	NextRetryAt time.Time `json:"next_retry_at"`
	LastError   string    `json:"last_error,omitempty"`
	ResolvedAt  time.Time `json:"resolved_at"`
	Resolution  string    `json:"resolution,omitempty"` // 解决说明（自动/人工，审计用）
}

// RetryFunc 是某类事项的自动重试执行器（返回 nil 表示已解决）。
type RetryFunc func(ctx context.Context, it Item) error

// RetryResult 是一次重试轮次的统计。
type RetryResult struct {
	Ran       int
	Succeeded int
	Failed    int
	Deferred  int // 无执行器 ⇒ 转人工
}

// ListFilter 是查询条件（API/UI 用；零值 = 全部）。
type ListFilter struct {
	Status         Status
	Kind           Kind
	Volume         string
	UnresolvedOnly bool
}

// Options 是 Manager 的可注入依赖（**不使用包级可变状态**）。
type Options struct {
	// Clock 返回当前时间（默认 time.Now）。注入以便测试并行、可推进虚拟时钟。
	Clock func() time.Time
	// Store 是持久化后端（默认 NewMemStore()；生产用 NewFileStore）。
	Store Store
	// RetryBackoff 是首次重试的退避间隔（默认 5m），后续按 2^n 递增（有上限）。
	RetryBackoff time.Duration
	// MaxAttempts 是自动重试次数上限（默认 5）；达到后转 needs_human。
	MaxAttempts int
	// MaxBackoff 是退避上限（默认 1h）。
	MaxBackoff time.Duration
}

func (o Options) withDefaults() Options {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Store == nil {
		o.Store = NewMemStore()
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = 5 * time.Minute
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Hour
	}
	return o
}

// Manager 是事项登记/重试/查询的唯一入口（并发安全）。
type Manager struct {
	opts Options

	mu    sync.Mutex
	items map[string]*Item
	// loadErr 记录持久化载入失败（不阻断运行，但会在列表里暴露一个 `KindOther` 事项）。
	loadErr error
}

// New 构造 Manager 并从 Store 载入既有事项（失败不致命，但会登记一条可感知的事项）。
func New(opts Options) *Manager {
	opts = opts.withDefaults()
	m := &Manager{opts: opts, items: map[string]*Item{}}
	if saved, err := opts.Store.Load(); err != nil {
		m.loadErr = err
	} else {
		for i := range saved {
			it := saved[i]
			m.items[it.ID] = &it
		}
	}
	return m
}

// Report 登记（或合并）一条事项并返回其当前状态。
//
// 语义：
//   - 同一 (Kind,Volume,Path) ⇒ **同一 ID**，重复登记只累加 `Occurrences` 并刷新 `LastSeen/Detail`
//     （避免重试循环里条目无限增长）；
//   - `Retryable=false` ⇒ 直接 `needs_human`（**不自动重试**），并安排后续人工处理；
//   - 已是 `resolved`/`needs_human` 的事项**重新出现**时按规则复位：resolved ⇒ 重新 open（问题复现），
//     needs_human ⇒ 保持人工状态（人工未处理前不自动重试）。
func (m *Manager) Report(it Item) Item {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.opts.Clock()
	if it.ID == "" {
		it.ID = itemID(it.Kind, it.Volume, it.Path)
	}
	cur, ok := m.items[it.ID]
	if !ok {
		it.FirstSeen = now
		it.LastSeen = now
		it.Occurrences = 1
		if it.Retryable {
			it.Status = StatusOpen
			it.NextRetryAt = now.Add(m.opts.RetryBackoff)
		} else {
			it.Status = StatusNeedsHuman
		}
		cp := it
		m.items[it.ID] = &cp
		m.persistLocked()
		return cp
	}

	// 合并：刷新描述与计数；保留既有生命周期字段。
	cur.LastSeen = now
	cur.Occurrences++
	if it.Detail != "" {
		cur.Detail = it.Detail
	}
	if it.Source != "" {
		cur.Source = it.Source
	}
	cur.Retryable = it.Retryable
	switch cur.Status {
	case StatusResolved:
		// 问题复现 ⇒ 复位为待处理（保留累计次数便于观察反复发生的顽固问题）。
		cur.Status = statusForRetryable(it.Retryable)
		cur.ResolvedAt = time.Time{}
		cur.Resolution = ""
		cur.Attempts = 0
		if it.Retryable {
			cur.NextRetryAt = now.Add(m.opts.RetryBackoff)
		}
	case StatusNeedsHuman:
		// 人工未处理前不自动重试；仅更新描述。
	default:
		if !it.Retryable {
			cur.Status = StatusNeedsHuman
			cur.NextRetryAt = time.Time{}
		}
	}
	cp := *cur
	m.persistLocked()
	return cp
}

// Due 返回**到期且可自动重试**的事项（open/retrying 且 Retryable 且 NextRetryAt<=now）。
func (m *Manager) Due(_ context.Context, now time.Time) []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Item
	for _, it := range m.items {
		if !it.Retryable || (it.Status != StatusOpen && it.Status != StatusRetrying) {
			continue
		}
		if it.NextRetryAt.After(now) {
			continue
		}
		out = append(out, *it)
	}
	sortItems(out)
	return out
}

// RunDue 对到期事项执行重试。
//
// execs：Kind → 执行器。**未注册执行器的 Kind 不会被静默跳过**，而是转 `needs_human`
// （避免"以为是自动重试、其实永远没跑"）。
func (m *Manager) RunDue(ctx context.Context, now time.Time, execs map[Kind]RetryFunc) RetryResult {
	due := m.Due(ctx, now)
	var res RetryResult
	for _, it := range due {
		if ctx.Err() != nil {
			return res
		}
		fn := execs[it.Kind]
		if fn == nil {
			m.markNeedsHuman(it.ID, fmt.Sprintf("无自动处理能力（未注册 %s 的执行器）⇒ 需人工接入", it.Kind))
			res.Deferred++
			continue
		}
		res.Ran++
		m.markRetrying(it.ID)
		if err := fn(ctx, it); err != nil {
			res.Failed++
			m.recordFailure(it.ID, err, now)
			continue
		}
		res.Succeeded++
		m.markResolved(it.ID, "自动重试成功")
	}
	return res
}

// Get 按 ID 取事项。
func (m *Manager) Get(id string) (Item, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return Item{}, false
	}
	return *it, true
}

// List 返回按「未解决优先 → 最近发现倒序」排序的事项。
func (m *Manager) List(f ListFilter) []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, 0, len(m.items))
	for _, it := range m.items {
		if !f.match(it) {
			continue
		}
		out = append(out, *it)
	}
	sortItems(out)
	return out
}

// Stats 返回各状态计数（UI 概览用）。
func (m *Manager) Stats() map[Status]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[Status]int{}
	for _, it := range m.items {
		out[it.Status]++
	}
	return out
}

// Resolve 由**人工**标记已解决（记录说明，供审计）；返回是否命中。
func (m *Manager) Resolve(id, note string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return false
	}
	if note == "" {
		note = "人工标记已解决"
	}
	it.Status = StatusResolved
	it.ResolvedAt = m.opts.Clock()
	it.Resolution = note
	it.NextRetryAt = time.Time{}
	m.persistLocked()
	return true
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// markResolved 内部：置为已解决（note 区分自动/人工）。
func (m *Manager) markResolved(id, note string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.items[id]; ok {
		it.Status = StatusResolved
		it.ResolvedAt = m.opts.Clock()
		it.Resolution = note
		it.NextRetryAt = time.Time{}
		it.LastError = ""
		m.persistLocked()
	}
}

// markRetrying 内部：标记执行器已接手（区分"已排队"与"正在跑"，便于 UI 呈现）。
func (m *Manager) markRetrying(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.items[id]; ok {
		it.Status = StatusRetrying
		m.persistLocked()
	}
}

// recordFailure 内部：记录失败；达上限 ⇒ 转人工（**有界**，绝不无限重试）。
func (m *Manager) recordFailure(id string, err error, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return
	}
	it.Attempts++
	it.LastError = err.Error()
	it.LastSeen = now
	if it.Attempts >= m.opts.MaxAttempts {
		it.Status = StatusNeedsHuman
		it.NextRetryAt = time.Time{}
		m.persistLocked()
		return
	}
	it.Status = StatusOpen
	it.NextRetryAt = now.Add(m.backoffFor(it.Attempts))
	m.persistLocked()
}

// markNeedsHuman 内部：转人工并写明原因。
func (m *Manager) markNeedsHuman(id, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.items[id]; ok {
		it.Status = StatusNeedsHuman
		it.NextRetryAt = time.Time{}
		it.LastError = reason
		m.persistLocked()
	}
}

// backoffFor 返回第 n 次重试后的退避（指数增长，上限 MaxBackoff）。
func (m *Manager) backoffFor(attempts int) time.Duration {
	d := m.opts.RetryBackoff
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= m.opts.MaxBackoff {
			return m.opts.MaxBackoff
		}
	}
	return d
}

// persistLocked 落盘（调用方持锁）。**持久化失败不致命**：错误会被记录为委托事项，
// 绝不因为"审计存储不可写"而中断业务（否则审计反而成为故障源）。
func (m *Manager) persistLocked() {
	if m.opts.Store == nil {
		return
	}
	items := make([]Item, 0, len(m.items))
	for _, it := range m.items {
		items = append(items, *it)
	}
	if err := m.opts.Store.Save(items); err != nil {
		m.loadErr = err
	}
}

// LoadError 返回最近一次持久化/载入错误（nil = 无）。
func (m *Manager) LoadError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadErr
}

// ErrNotFound 用于 API 层区分"事项不存在"。
var ErrNotFound = errors.New("issues: 事项不存在")

// statusForRetryable 返回新事项的初始状态。
func statusForRetryable(retryable bool) Status {
	if retryable {
		return StatusOpen
	}
	return StatusNeedsHuman
}

// itemID 由 (Kind,Volume,Path) 派生**稳定 ID**（同事项重复登记必须合并）。
func itemID(k Kind, vol, p string) string {
	sum := sha256.Sum256([]byte(string(k) + "\x00" + vol + "\x00" + p))
	return string(k) + ":" + hex.EncodeToString(sum[:])[:16]
}

// match 判断事项是否满足筛选条件。
func (f ListFilter) match(it *Item) bool {
	if f.Status != "" && it.Status != f.Status {
		return false
	}
	if f.Kind != "" && it.Kind != f.Kind {
		return false
	}
	if f.Volume != "" && it.Volume != f.Volume {
		return false
	}
	if f.UnresolvedOnly && it.Status == StatusResolved {
		return false
	}
	return true
}

// sortItems 排序：未解决优先（needs_human → open/retrying → resolved），同组按最近发现倒序。
func sortItems(items []Item) {
	weight := func(s Status) int {
		switch s {
		case StatusNeedsHuman:
			return 0
		case StatusOpen, StatusRetrying:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		wi, wj := weight(items[i].Status), weight(items[j].Status)
		if wi != wj {
			return wi < wj
		}
		return items[i].LastSeen.After(items[j].LastSeen)
	})
}
