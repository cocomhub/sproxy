// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import "time"

// indexSaveLoop 周期保存搜索索引快照（index_save_interval > 0 时由 RegisterRoutes
// 启动；Close() 关 indexSaveStop）。与 mirrorVolumeLoop 同构（ticker + stop channel）。
// 启动时先保存一次（把载入态固化）；周期保存写路径增量变更。
func (h *Handlers) indexSaveLoop(interval time.Duration) {
	h.saveIndexSnapshots()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.indexSaveStop:
			return
		case <-ticker.C:
			h.saveIndexSnapshots()
		}
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
