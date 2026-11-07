// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// 归档 DTO 的唯一实现已迁入 cloudtasks（P1-c S3a）；装配层与既有测试经类型别名继续引用，
// 响应结构体因此仍只有一份定义（JSON 字段顺序/标签零变化）。
//
// S4 起 tar.gz 打包助手（createTarGz/createMultiFileTarGz/openArchiveOutput/errArchiveExists）
// 也已迁入 cloudtasks/archive_tar.go，本文件只保留**配额预留/对账**两个非响应核心——它们把
// 失败映射为 HTTP 状态码 + 中文文案，属装配层语义（错误面由 HTTP 层定义，核心只消费结果）。
type (
	CloudArchiveRequest      = cloudtasks.CloudArchiveRequest
	CloudArchiveBatchRequest = cloudtasks.CloudArchiveBatchRequest
	CloudArchiveResult       = cloudtasks.CloudArchiveResult
)

// cloudArchiveReservePlaceholder 云归档打包预留占位（100MB）。
// tar 头（≤512B/文件）+ gzip 可能在极少数情况下产生少量膨胀，预留后按实际大小对账。
const cloudArchiveReservePlaceholder = int64(100 * 1024 * 1024)

// 云归档编排入口（P1-c S3a 已迁入 pkg/server/cloudtasks）。装配层只保留 owner 解析
// 适配器（`ActorFrom` 的 ctx key 是装配层私有，核心显式接收 owner）。
func (h *Handlers) archiveTaskHandler() http.HandlerFunc {
	return h.cloudTaskHandler(h.cloudTaskService().ArchiveTask)
}

func (h *Handlers) archiveBatchHandler() http.HandlerFunc {
	return h.cloudTaskHandler(h.cloudTaskService().ArchiveBatch)
}

// archiveSettleQuota 按磁盘实际大小对账预留配额的**非响应核心**（单任务 / 批量 / 组归档
// 共用）：scope 路径 = Reconcile(actual) + Commit()（多预留部分自动归还）；storageMgr 回退
// 保留 3 步（Release(pre)+TryReserve(actual)）。失败返回 status+message（响应由调用方写出，
// 文案与迁移前逐字一致）。
func (h *Handlers) archiveSettleQuota(root *storage.Root, rel string, res *quota.Settlement, pre int64, logger *slog.Logger) (actual int64, status int, message string) {
	if res != nil {
		info, statErr := root.Stat(rel)
		if statErr != nil {
			// stat 失败（罕见）：释放预留避免永久挂账（归档文件已落盘，下次扫描校准全局账本）。
			res.Release()
			return 0, 0, ""
		}
		actual = info.Size()
		commitArchiveSettlement(res, pre, actual, logger)
		return actual, 0, ""
	}
	if h.storageMgr == nil {
		return 0, 0, ""
	}
	h.storageMgr.Release(pre, capacity.CategoryCloud)
	info, statErr := root.Stat(rel)
	if statErr != nil {
		return 0, 0, ""
	}
	actual = info.Size()
	if rErr := h.storageMgr.TryReserve(actual, capacity.CategoryCloud); rErr != nil {
		logger.Error("storage full, removing archive to keep ledger consistent", "error", rErr)
		_ = root.Remove(rel)
		return 0, http.StatusInsufficientStorage, fmt.Sprintf("insufficient storage for archive: %v", rErr)
	}
	return actual, 0, ""
}

// archiveReserveQuota 是归档预留的**非响应核心**（单任务 / 批量 / 组归档共用）：先校验
// cloud_archive_max_bytes 上限，再按「scope 优先（ReserveWait 排队）、storageMgr 回退」预留。
// 失败返回 status+message（文案与迁移前逐字一致）；成功返回结算句柄（nil = 回退/未装配）
// 与预留量 pre。
func (h *Handlers) archiveReserveQuota(ctx context.Context, owner string, totalSourceSize int64) (*quota.Settlement, int64, int, string) {
	if maxBytes := (&cloudTasksAdapter{h: h}).MaxArchiveBytes(); maxBytes > 0 && totalSourceSize > maxBytes {
		return nil, 0, http.StatusBadRequest, fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", totalSourceSize, maxBytes)
	}
	pre := totalSourceSize + cloudArchiveReservePlaceholder
	// P5 收敛：双轨 TryReserve（storageMgr + Scope 同时预留同量字节）改为二选一——
	// 生产环境 scope 恒非 nil（全局兜底由 Scope 父链生效），storageMgr 仅作回退。
	// P1-b：scope 路径的「预留 → 对账 → 提交/回滚」改走 quota.Settlement（协议唯一实现）；
	// ReserveWait 保留「容量临时不足 ⇒ 排队等释放」的能力（#963 锁定的语义，不得退化为立即 507）。
	if scope := h.quotaBucketFor(owner, "archive"); scope != nil {
		st := quota.NewSettlement(scope.Pool())
		// h.quotaWaitTimeout = 0（生产恒 0）⇒ ReserveWait 内部按 quota.DefaultWaitTimeout（30s），
		// 语义不变。此前此处恒传 quota.DefaultWaitTimeout，测试注入的等待上限被忽略（#963 记录）。
		if reserveErr := st.ReserveWait(ctx, pre, h.quotaWaitTimeout); reserveErr != nil {
			// 失败时账本与句柄状态均不变（ReserveWait 契约）⇒ 直接丢弃句柄，无预留泄漏。
			return nil, 0, http.StatusInsufficientStorage, fmt.Sprintf("insufficient storage: %v", reserveErr)
		}
		return st, pre, 0, ""
	} else if h.storageMgr != nil {
		if reserveErr := h.storageMgr.WaitReserve(ctx, pre, capacity.CategoryCloud, capacity.DefaultWaitTimeout); reserveErr != nil {
			return nil, 0, http.StatusInsufficientStorage, fmt.Sprintf("insufficient storage: %v", reserveErr)
		}
	}
	// 回退路径（storageMgr）或未装配账本：无结算句柄需定案（回退预留按 pre 释放）。
	return nil, pre, 0, ""
}
