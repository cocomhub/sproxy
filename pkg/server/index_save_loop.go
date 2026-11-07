// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import "time"

// indexSaveDebounce 是「写后落盘」的防抖粒度：写路径置脏后最多这么久即落盘，
// 把崩溃窗口从周期 interval（默认 5m）缩到秒级（审计 2026-10-29）。
const indexSaveDebounce = time.Second

// indexSaveLoop 周期保存搜索索引快照（index_save_interval > 0 时由 RegisterRoutes 启动；
// Close() 关 indexSaveStop）。与 mirrorVolumeLoop 同构（ticker + stop channel）。
//
// **启动时不保存**：此前启动先 save 一次，会把「刚载入的陈旧快照」重新固化为当前态
// （崩溃丢失的增量再也无法通过重建恢复）。
//
// 两条节奏：
//   - debounce（1s）只落「写后置脏」的 owner ⇒ 崩溃窗口 ≤1s；
//   - periodic（配置 interval）全量固化，语义与既有配置一致。
func (h *Handlers) indexSaveLoop(interval time.Duration) {
	h.indexSaveLoopWithDebounce(interval, indexSaveDebounce)
}

// indexSaveLoopWithDebounce 是 indexSaveLoop 的可测形态（debounce 可注入）。
func (h *Handlers) indexSaveLoopWithDebounce(interval, debounce time.Duration) {
	debounceTicker := time.NewTicker(debounce)
	defer debounceTicker.Stop()
	periodic := time.NewTicker(interval)
	defer periodic.Stop()
	for {
		select {
		case <-h.indexSaveStop:
			h.saveDirtyIndexSnapshots() // 退出前尽力落盘（不固化未变更的 owner）
			return
		case <-debounceTicker.C:
			h.saveDirtyIndexSnapshots()
		case <-periodic.C:
			h.saveIndexSnapshots()
		}
	}
}

// saveDirtyIndexSnapshots 只落盘「写后置脏」的索引快照（无变更 ⇒ 零 I/O）。
func (h *Handlers) saveDirtyIndexSnapshots() {
	svc := h.fileService()
	if svc == nil {
		return
	}
	if n := svc.SaveDirtyIndexSnapshots(); n > 0 {
		h.logger.Debug("索引快照增量保存", "owners", n)
	}
}

// saveIndexSnapshots 保存全部已构建 owner 的索引快照（幂等；filesSvc 未装配时 no-op）。
func (h *Handlers) saveIndexSnapshots() {
	svc := h.fileService()
	if svc == nil {
		return
	}
	if n := svc.SaveIndexSnapshots(); n > 0 {
		h.logger.Debug("索引快照周期保存", "owners", n)
	}
}
