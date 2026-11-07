// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_settlement_wiring.go 是云归档（单任务 / 批量 / 组）与 `quota.Settlement` 之间的接线。
//
// 「预留 → 写入 → 对账 → 提交/回滚」的协议本身由 pkg/quota/settlement.go **唯一实现**；装配层
// 只剩两件事：① 成功路径把预留收敛到落盘真值后提交；② 失败路径释放预留。本文件收口这两件事，
// 避免三个归档入口各写一遍——顺序错或漏回滚会直接造成配额泄漏（预留永不归还 ⇒ 该租户假满）
// 或超卖（未对账就提交 ⇒ 账本小于磁盘）。

import (
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// releaseArchiveReservation 释放云归档预留，与预留的「二选一」形态对称：
// scope 路径（st 非 nil）→ Settlement.Release（未提交则全额归还，幂等）；storageMgr 回退
// 预留 → 按 pre 释放。不 double release：两条路径互斥，同一归档只走其中一条。
func releaseArchiveReservation(st *quota.Settlement, sm *capacity.StorageManager, pre int64) {
	if st != nil {
		st.Release()
	} else if sm != nil {
		sm.Release(pre, capacity.CategoryCloud)
	}
}

// commitArchiveSettlement 把 scope 路径的预留对账到落盘真值并提交（迁移前是单步
// `Reservation.Commit(actual)`）。
//
// Reconcile 是「预留 → 实际」的唯一收敛入口：实际小于预留 ⇒ 差额归还并唤醒等待者；实际大于
// 预留 ⇒ 需追加预留，容量不足时返回错误（此时预留保持原值、状态仍为 open）。该失败分支仍按
// **可提交量**提交：迁移前的 Commit(actual) 永不失败 ⇒「打包已成功但追加记账失败」不能变成
// 507，差额由周期对账校准。该分支实际不可达——预留 = 源总量 + 100 MiB 占位，tar/gzip 头开销
// 远小于占位。
func commitArchiveSettlement(st *quota.Settlement, pre, actual int64, logger *slog.Logger) {
	if err := st.Reconcile(actual); err != nil {
		logger.Error("archive grew beyond quota reservation; committing reserved amount",
			"reserved", pre, "actual", actual, "error", err)
	}
	if err := st.Commit(); err != nil {
		logger.Error("failed to commit archive quota settlement", "error", err)
	}
}
