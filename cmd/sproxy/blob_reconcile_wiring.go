// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// blob_reconcile_wiring.go —— ⑤⑥ 装配接线：把 baidupcs 的「blob md5 未收敛」待复查项
// 接到既有 `pkg/server/issues` 闭环（登记 → 5min 退避重试 → 收敛解除 / 有界转人工），
// 并提供**启动兜底扫描**（把副本保留但登记丢失的孤儿重新纳入处理）。
//
// 分工（与设计文档 §3.3/§7.8 一致）：
//   - `baidupcs.Storage`：判定「内容已确认在云端但 md5 未权威」⇒ 保留副本 + 通过 `ReconcileReporter` 上报；
//     并提供 `RetryBlobMD5Reconcile`（重传 2 次 + 复查 + 终态清理）与 `PendingReconcile`（扫描）；
//   - 本文件：把上报落到 issues（`KindBlobMD5Unconverged`），把 issues 执行器路由回对应卷的 Storage。
//
// **仅 baidu 后端启用**：本接线只由 baidupcs 后端构造时注册/注入 ⇒ 其它后端天然不受影响。
import (
	"context"
	"fmt"
	"sync"

	"github.com/cocomhub/sproxy/pkg/server/issues"
	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs"
)

// issueReporter 是事项中心的**窄接口**（只取登记能力，便于测试替身与解耦）。
type issueReporter interface {
	ReportStorageIssue(volume, kind, path, detail string, retryable bool)
}

// setupBaidupcsReconcileWiring 由装配根调用（**必须在卷后端类型注册/卷装配之前**）：
// 把「内容已确认在云端但 md5 未权威」的待复查项登记到事项中心，并把重试路由回对应卷。
// ic 为 nil ⇒ 仍建立接线器（保留副本可被启动扫描发现），但不登记事项。
func setupBaidupcsReconcileWiring(ic issueReporter) *blobReconcileWiring {
	reconcileWiring = newBlobReconcileWiring(func(volume, rel, localCopy string) {
		if ic == nil {
			return
		}
		ic.ReportStorageIssue(volume, string(issues.KindBlobMD5Unconverged), rel,
			fmt.Sprintf("内容已确认在云端（block_list 逐块对账通过）但远端 md5 未权威（百度分片上传语义）。"+
				"已保留本地副本（%s），将连续上传 2 次并复查；达重试上限转人工。", localCopy), true)
	})
	return reconcileWiring
}

// blobReconcileWiring 是装配期的「blob md5 待复查」接线器（按卷名路由）。
//
// 为什么按卷名存（而不是拿卷的 FS 下探）：重传动作是 baidupcs 后端特化的（需要
// `RetryBlobMD5Reconcile` 这一非通用能力），而后端构造点（`newBaidupcsBackendWithFactory`）
// 是唯一同时持有 `*baidupcs.Storage` 与卷名的地方 ⇒ 在此登记最直接、无需给通用 FS 加特化能力
// （也避免装饰器链漏转发该能力）。
type blobReconcileWiring struct {
	mu   sync.Mutex
	caps map[string]baidupcs.ReconcileCapable
	// report 把待复查项登记到事项中心（装配根注入；nil ⇒ 只保留副本不登记，由后端侧告警）。
	// 参数：(卷名, 卷内 rel, 本地副本路径)。**事项以 rel 作为 Path**（稳定 ID、重复上报合并），
	// 副本路径仅用于人类可读的 Detail/排查。
	report func(volume, rel, localCopy string)
}

// newBlobReconcileWiring 构造接线器；report 可为 nil（降级：不登记，只在后端侧告警）。
func newBlobReconcileWiring(report func(volume, rel, localCopy string)) *blobReconcileWiring {
	return &blobReconcileWiring{caps: map[string]baidupcs.ReconcileCapable{}, report: report}
}

// reporterFor 返回该卷的上报口（在 `StorageConfig.Reconcile` 注入）。
// 上报路径固定为本接线器 ⇒ 保证"上报"与"重试路由"同一份卷名口径。
func (w *blobReconcileWiring) reporterFor(volume string) baidupcs.ReconcileReporter {
	if w == nil {
		return nil
	}
	return wiringReporter{w: w, volume: volume}
}

// register 登记某卷的待复查能力（后端构造点调用；对象不支持则忽略）。
func (w *blobReconcileWiring) register(volume string, v any) {
	if w == nil || v == nil {
		return
	}
	cap, ok := baidupcs.AssertReconcileCapable(v)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.caps[volume] = cap
}

// retry 执行一次重试（issues 执行器入口）。
//
// 副本**按 (volume, rel) 反查**（而不是让事项携带随机副本路径）：
// 这样 `Item.Path` 可以是稳定的 rel ⇒ 同一文件重复上报合并为同一条事项，不会堆积重复条目。
// 找不到副本 ⇒ 返回错误（无源可传，交人工），绝不静默成功。
func (w *blobReconcileWiring) retry(ctx context.Context, volume, rel string) error {
	if w == nil {
		return fmt.Errorf("blob md5 重试接线未装配（需人工处理）")
	}
	w.mu.Lock()
	cap, ok := w.caps[volume]
	w.mu.Unlock()
	if !ok {
		return fmt.Errorf("卷 %q 无 blob md5 重试能力（未装配/非 baidupcs 后端），需人工处理", volume)
	}
	localCopy := ""
	for _, it := range cap.PendingReconcile() {
		if it.Key == rel {
			localCopy = it.LocalCopy
			break
		}
	}
	if localCopy == "" {
		return fmt.Errorf("卷 %q 中 rel=%q 的保留副本不存在（可能已被清理/进程重启丢失）⇒ 无源可传，需人工处理", volume, rel)
	}
	return cap.RetryBlobMD5Reconcile(ctx, rel, localCopy)
}

// scanAndReport 是**启动兜底扫描**：把各卷 `PendingReconcile()` 里的副本重新登记为事项
// （不依赖 issues 持久化是否成功——进程被强杀/持久化失败时仍能收敛）。
// 返回重新登记的条数。`rel` 为空的条目（pending.json 损坏/缺失）**跳过并计数**，由人工介入。
func (w *blobReconcileWiring) scanAndReport() (reported, skipped int) {
	if w == nil {
		return 0, 0
	}
	w.mu.Lock()
	caps := make(map[string]baidupcs.ReconcileCapable, len(w.caps))
	for k, v := range w.caps {
		caps[k] = v
	}
	w.mu.Unlock()

	for volume, cap := range caps {
		for _, it := range cap.PendingReconcile() {
			if it.Key == "" || it.LocalCopy == "" {
				skipped++ // 无法定位 ⇒ 不猜（交人工），绝不静默删
				continue
			}
			if w.report != nil {
				w.report(volume, it.Key, it.LocalCopy)
			}
			reported++
		}
	}
	return reported, skipped
}

// wiringReporter 是 `baidupcs.ReconcileReporter` 的装配实现（绑定卷名）。
type wiringReporter struct {
	w      *blobReconcileWiring
	volume string
}

// ReportBlobMD5Pending 把待复查项登记到事项中心（report 为 nil ⇒ 不登记，后端侧已告警）。
func (r wiringReporter) ReportBlobMD5Pending(rel, localCopy string) {
	if r.w == nil || r.w.report == nil {
		return
	}
	r.w.report(r.volume, rel, localCopy)
}
