// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"time"
)

// totpPendingTTL 是 TOTP 注册 pending 的有效期（10 分钟）。用户在期限内录入
// Authenticator 并完成首次登录提交；超时自动回收（AK 可复用，admin 不被占用）。
const totpPendingTTL = 10 * time.Minute

// totpPendingMax 是 pending 表元素上限（防注册洪泛无界增长；池满淘汰最旧）。
const totpPendingMax = 1024

// totpPendingEntry 是单条 TOTP 注册 pending 记录。
type totpPendingEntry struct {
	// AK 是注册生成的 AccessKey（pending 标识；登录提交时用 AK 定位）。
	AK string
	// Owner 是注册请求的 owner（幂等检查键：同 owner 已有 pending → 409）。
	Owner string
	// TOTPSecret 是 pending 阶段的账号级 TOTP secret（20B；提交时写入 ring Key）。
	TOTPSecret []byte
	// ExpiresAt 是 pending 过期时间（超时自动回收）。
	ExpiresAt time.Time
	// FailCount 是 pending 提交（登录）的失败次数（达阈值回收，防爆破 pending）。
	FailCount int
}

// totpPendingTable 是 TOTP 注册 pending 表（两段式提交的 pending 态存储）。
// 纯内存态：不落盘、重启即清（与 nonce 池同生命周期）。并发安全（sync.Mutex）。
// 语义：
//   - Add(ak, owner, secret)：插入（池满淘汰最旧）；返回 (ok, conflictOwner)——
//     同 owner 已有活跃 pending → conflict（不覆盖）；
//   - 首 admin 单槽由调用方（handler）用 HasAdminPending + ring 判定：无 admin 时
//     只允许一条 pending；
//   - Commit(ak)：登录成功提交时移除 pending（ring 写入由 handler 负责）。
type totpPendingTable struct {
	mu sync.Mutex
	m  map[string]totpPendingEntry // key = AK
	// nowFn 可注入时钟（测试）；nil → time.Now。
	nowFn func() time.Time
}

func newTotpPendingTable() *totpPendingTable {
	return &totpPendingTable{m: make(map[string]totpPendingEntry)}
}

// SetClock 注入时钟（测试用，仿 totpNoncePool.SetClock）。
func (t *totpPendingTable) SetClock(fn func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nowFn = fn
}

func (t *totpPendingTable) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

// pruneLocked 惰性清理已过期条目（调用方持有 mu）。
func (t *totpPendingTable) pruneLocked(now time.Time) {
	for ak, e := range t.m {
		if !e.ExpiresAt.After(now) {
			delete(t.m, ak)
		}
	}
}

// Add 插入 pending（同 AK 已存在 → 覆盖刷新；同 owner 已有活跃 pending → conflict）。
// 返回 (ok, conflict)：ok=false 且 conflict=true 表示同 owner 冲突（幂等 409 语义）。
// adminSlot=true 时强制「全表最多一条」：ring 无 admin 的首 admin 阶段只允许一个候选
// （防并发 pending 抢 admin）。池满时淘汰最旧条目（按 ExpiresAt 升序）。
func (t *totpPendingTable) Add(ak, owner string, secret []byte, adminSlot bool) (ok bool, conflict bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.pruneLocked(now)

	// 首 admin 单槽（原子）：已有任一活跃 pending → 拒绝（409 由调用方按 conflict 处理）。
	if adminSlot && len(t.m) > 0 {
		return false, true
	}

	// 同 owner 已有活跃 pending（不同 AK）→ 冲突。
	for _, e := range t.m {
		if e.AK != ak && e.Owner == owner && e.ExpiresAt.After(now) {
			return false, true
		}
	}
	// 池满淘汰最旧。
	if len(t.m) >= totpPendingMax {
		var oldestAK string
		var oldest time.Time
		first := true
		for a, e := range t.m {
			if first || e.ExpiresAt.Before(oldest) {
				oldestAK, oldest, first = a, e.ExpiresAt, false
			}
		}
		if oldestAK != "" {
			delete(t.m, oldestAK)
		}
	}
	t.m[ak] = totpPendingEntry{
		AK:         ak,
		Owner:      owner,
		TOTPSecret: append([]byte(nil), secret...),
		ExpiresAt:  now.Add(totpPendingTTL),
	}
	return true, false
}

// Get 返回活跃 pending（未过期；不存在/已过期 → (零值, false)）。
func (t *totpPendingTable) Get(ak string) (totpPendingEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.pruneLocked(now)
	e, ok := t.m[ak]
	if !ok || !e.ExpiresAt.After(now) {
		return totpPendingEntry{}, false
	}
	return e, true
}

// RecordFail 记录一次 pending 提交失败（达阈值返回 true = 应回收）。
func (t *totpPendingTable) RecordFail(ak string, limit int) (shouldRemove bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.pruneLocked(now)
	e, ok := t.m[ak]
	if !ok || !e.ExpiresAt.After(now) {
		return true // 已不存在/过期 → 视为应移除
	}
	e.FailCount++
	t.m[ak] = e
	return e.FailCount >= limit
}

// Remove 移除 pending（登录成功提交 / 达阈值回收时调用）。
func (t *totpPendingTable) Remove(ak string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, ak)
}

// HasActivePending 报告是否存在任一活跃 pending（首 admin 单槽判定用）。
func (t *totpPendingTable) HasActivePending() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.pruneLocked(now)
	for _, e := range t.m {
		if e.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}

// Len 返回当前 pending 条目数（测试/诊断）。
func (t *totpPendingTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(t.now())
	return len(t.m)
}
