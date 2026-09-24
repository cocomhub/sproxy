// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// handlers_lifecycle.go 是**停服收口**：Close（停 uploading 清理 goroutine、停各租户 UploadStore、
// 停存储/云下载/分享后台任务、hub 状态最终落盘、关闭多租户存储根）与 snapshotCurrent（供最终落盘
// 使用的 hub 快照构建）。
//
// 拆分说明见 handlers.go 顶部。

package server

import (
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// Close 释放 Handlers 持有的后台资源：停止 UploadStore 的 persist/cleanup goroutine 和 StorageManager 的定期扫描。
// 在进程退出前应调用一次（通常通过 defer h.Close()）。多次调用是安全的。
// 关闭顺序：先关 uploadingFiles 清理 goroutine，再关 UploadStore（后者可能还有 uploading 操作引用其 session）。
// 返回值当前恒为 nil：各子组件的 Stop/Close（UploadStore.Stop / StorageManager.Stop /
// CloudDownloadManager.Close / ShareStore.Stop / tenants.Close / volSet.Close）签名均不返回
// error，唯一可失败的 hubPersist.FlushFn 已在本方法内就地记 Error 日志。若要聚合关闭错误，
// 需先扩这些组件的签名，属独立改造——故不在此预留半成品（原 TODO 审计结论，2026-09-14）。
func (h *Handlers) Close() error {
	// 先关闭 uploadingFiles 清理 goroutine（经统一调度器），确保不再引用
	// uploadStore session；同时停止版本/回收站周期 GC 与分享清理（见 scheduler 装配）。
	// 手工构造的旧装配路径未装配 scheduler（nil）则跳过——兼容零回归。
	h.closeOnce.Do(func() {
		if h.scheduler != nil {
			h.scheduler.Stop()
		}
		if h.uploadingStop != nil {
			close(h.uploadingStop)
		}
		if h.rotationStop != nil {
			close(h.rotationStop)
		}
		if h.mirrorStop != nil {
			close(h.mirrorStop)
		}
		if h.tierStop != nil {
			close(h.tierStop)
		}
		if h.indexSaveStop != nil {
			close(h.indexSaveStop)
		}
		if h.alertEngine != nil {
			h.alertEngine.Close()
		}
	})
	h.rotationWg.Wait()
	h.mirrorWg.Wait()
	h.tierWg.Wait()
	h.indexSaveWg.Wait()
	// 关闭后保存一次（把最终写路径增量固化，重启免全量 WalkDir）。
	h.saveIndexSnapshots()

	// 停止所有 per-tenant UploadStore（persist/cleanup goroutine）。
	// 保留 uploadStores map（不清空）：/healthz 探活需能看到已停止的 store 并返回 503；
	// Stop 幂等（stopOnce），重复 Close 安全。
	h.tenantMu.Lock()
	for _, us := range h.uploadStores {
		if us != nil {
			us.Stop()
		}
	}
	h.tenantMu.Unlock()

	if h.storageMgr != nil {
		h.storageMgr.Stop()
	}
	if h.cloudMgr != nil {
		h.cloudMgr.Close()
	}
	// 审计落盘：优雅停服 flush 并关闭日志文件句柄（Windows 句柄释放，TempDir cleanup 可删）。
	if h.auditStore != nil {
		_ = h.auditStore.Close()
		h.auditStore = nil
	}
	if h.shareStore != nil {
		h.shareStore.Stop()
	}
	if h.relayStream != nil && h.relayStream.forwarder != nil {
		h.relayStream.forwarder.Close()
	}
	// hub 状态持久化器最终 flush：优雅停服前把最后一次注册/信令变更落盘。
	// 快照生成在 Persister 锁内执行（FlushFn 持有 p.mu 再调 snapshotCurrent），
	// 避免停服时节点下线与快照生成之间的竞态导致旧快照覆盖新状态（I1）。
	if h.hubPersist != nil {
		if err := h.hubPersist.FlushFn(func() *hub.Snapshot { return h.snapshotCurrent() }); err != nil {
			h.logger.Error("shutdown: hub 状态最终落盘失败", "err", err)
		}
	}
	// 关闭多租户存储根：先关各租户子根（默认卷缓存的租户子根；非默认卷的随 volSet.Close），
	// 再关卷集合根（含默认卷根 = globalRoot）。置 nil 防重复 Close。volSet == nil（手工构造的
	// 旧装配路径）回落直接关 globalRoot（既有行为）。
	_ = h.tenants.Close()
	if h.volSet != nil {
		_ = h.volSet.Close()
		h.volSet = nil
		h.globalRoot = nil
	} else if h.globalRoot != nil {
		_ = h.globalRoot.Close()
		h.globalRoot = nil
	}
	return nil
}

// snapshotCurrent 构建当前完整 hub 快照（节点 + 信令收件箱）。
// 命名不用 snapshotLocked：本函数自身不持任何锁（节点/队列锁在各 Snapshot 函数内
// 短临界区自行加解锁），避免误导调用方以为入参需预先持锁。
func (h *Handlers) snapshotCurrent() *hub.Snapshot {
	if h.routeTable == nil {
		return &hub.Snapshot{}
	}
	snap := hub.SnapshotRouteTable(h.routeTable)
	// M4：与 FlushSignal / onChange 一致，过滤孤儿收件箱，避免停服快照写入死信。
	snap.Messages = h.signalBroker.signalSnapshots()
	return snap
}
