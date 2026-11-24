// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// retryableCloudFailure 判断该失败类别是否值得自动重试。
//
// 依据（设计文档 2026-11-21 §3.3）：源不可达/限流/内容不一致可重试（内容不一致应换源）；
// 目标冲突/磁盘满/鉴权需人工处理，自动重试只会浪费时间并掩盖问题。
func retryableCloudFailure(class string) bool {
	switch class {
	case cloud.ErrClassSourceUnreachable, cloud.ErrClassThrottled, cloud.ErrClassContentMismatch,
		cloud.ErrClassTargetUnavailable:
		return true
	default:
		return false
	}
}

// ReportCloudTaskFailure 把云端下载失败翻译为一条事项（装配层注入 manager 的失败观察者）。
//
// 登记约定：`Volume`/`Path` = **转存目标**（稳定 ⇒ 同目标重复失败合并为一条事项，
// 且执行器可据 (volume, rel) 唯一重试）。无转存目标的任务按 URL 登记在 Volume="cloud" 下。
func (h *Handlers) ReportCloudTaskFailure(f cloud.TaskFailure) {
	center := h.IssueCenter()
	if center == nil {
		return
	}
	h.metrics.RecordCloudTaskFailed(f.ErrorClass)
	volume, path := f.Volume, f.Path
	if volume == "" {
		volume, path = "cloud", f.URL
	}
	detail := fmt.Sprintf("云端下载任务失败：task=%s 类别=%s 次数=%d 源=%s 错误=%s",
		f.TaskID, f.ErrorClass, f.Attempts, f.URL, f.Error)
	if f.ArtifactDir != "" {
		detail += fmt.Sprintf("；已保留产物目录=%s（未删除，可人工判断/换源复用）", f.ArtifactDir)
	}
	center.ReportStorageIssue(volume, string(issues.KindCloudDownloadFailed), path, detail,
		retryableCloudFailure(f.ErrorClass))
}

// RetryCloudDownload 执行一次云端下载失败重试（事项执行器注入）。
//
// 通过「转存目标卷+路径」定位失败任务并重试（复用 cloud.RetrySelector/RetryTasks），
// 与人工 `cloud-download retry --transfer-path` 完全同一内核。
func (h *Handlers) RetryCloudDownload(ctx context.Context, volume, rel string) error {
	if h.cloudMgr == nil {
		return fmt.Errorf("cloud manager 未装配，无法重试")
	}
	sel := cloud.RetrySelector{
		TransferVolume:          volume,
		TransferPath:            rel,
		IncludeCompletedDamaged: true,
		Limit:                   1,
	}
	retried, errs := h.cloudMgr.RetryTasks(sel, false)
	if len(retried) > 0 {
		return nil // 已提交重试（结果由新的失败/成功事实驱动）
	}
	if len(errs) > 0 {
		for _, err := range errs {
			return err
		}
	}
	return fmt.Errorf("未找到可重试的云端下载任务（volume=%s rel=%s）", volume, rel)
}

// SetCloudTaskFailureObserver 把「云端下载失败」观察者注入 cloud manager（幂等）。
//
// 时机：事项中心就绪后调用（manager 先于它构造，故用后置注入而非构造参数）。
func (h *Handlers) SetCloudTaskFailureObserver() {
	if h.cloudMgr == nil {
		return
	}
	h.cloudMgr.SetTaskFailureObserver(h.ReportCloudTaskFailure)
}
