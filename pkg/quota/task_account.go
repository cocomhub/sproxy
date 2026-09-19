// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import "sync"

// TaskAccount 持有某任务在 Scope 中的配额占用（reserved + committed）的唯一所有权。
// 生命周期显式：NewTaskAccount（立账）→ CommitUp（写盘边写边记）→ Release（终态释放，幂等）。
//
// 语义对齐 QuotaWriter 的记账核心（本类型是其抽取，供 cloud 下载任务收敛「配额所有权
// 多记账点 + 多释放点」的结构性问题）：reserved 是已立账预占（写后递减），committed 是
// 已确认的实际写入量；Release 一次性回拨两者。内部自锁（叶锁），与写盘并发安全——
// 释放与 CommitUp 并发时由 mu 串行化，杜绝「释放后被 commit 抬回」或「释放点与所有权
// 不匹配」的账本泄漏窗口。
type TaskAccount struct {
	mu        sync.Mutex
	scope     *Scope
	reserved  int64 // 已立账预占（写后递减）
	committed int64 // 已确认 committed 的实际写入量
}

// NewTaskAccount 创建针对 scope 的 TaskAccount，写前预留 estimate（<=0 时用 1 GiB 占位）。
// 预留失败返回 ErrStorageFull；调用方不持有预留句柄（由 TaskAccount 自行管理）。
func NewTaskAccount(s *Scope, estimate int64) (*TaskAccount, error) {
	if estimate <= 0 {
		estimate = placeholderReserve
	}
	if err := s.pool.reserveUp(estimate); err != nil {
		return nil, err
	}
	return &TaskAccount{scope: s, reserved: estimate}, nil
}

// CommitUp 把本次写入量从 reserved 划入 committed；预留不够自动补留。
// 补留失败返回 ErrStorageFull 且不改状态（reserved/committed 保持调用前的值）。
func (a *TaskAccount) CommitUp(n int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 {
		return nil
	}
	if n > a.reserved {
		if err := a.scope.pool.reserveUp(n - a.reserved); err != nil {
			return err
		}
		a.reserved = n // 补齐到本次需要
	}
	a.scope.pool.commitUp(n, n)
	a.reserved -= n
	a.committed += n
	return nil
}

// ReleaseReserve 释放剩余 reserve 但保留已 commit 部分（写失败保留 .partial 供续传：
// 已 commit 字节继续占账，未用 reserve 归还）。幂等：reserved 已归零时为空操作。
func (a *TaskAccount) ReleaseReserve() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseReserveLocked()
}

// Release 释放任务在 Scope 中的全部占用（committed + reserved），幂等。
// 与 QuotaWriter.Finish 不同：这里**不**做覆盖写对账（oldSize），只回拨本任务已立账的
// 全部字节——调用方（删除/取消/过期清理）以「任务账本归零」为语义。
func (a *TaskAccount) Release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.committed > 0 {
		a.scope.ReleaseUsage(a.committed)
		a.committed = 0
	}
	a.releaseReserveLocked()
}

// releaseReserveLocked 释放剩余 reserve（调用方须已持 a.mu）。
func (a *TaskAccount) releaseReserveLocked() {
	if a.reserved <= 0 {
		return
	}
	a.scope.pool.releaseUp(a.reserved)
	a.reserved = 0
}

// Committed 返回已确认 committed 的字节数（结算/对账前调用）。
func (a *TaskAccount) Committed() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.committed
}

// Reserved 返回已立账预占的字节数。
func (a *TaskAccount) Reserved() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserved
}
