// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_usage.go —— 卷用量的**单一查询面**（统一存储卷模型「接线」步，2026-10-16 用户裁定
// 「QuotaVolume 接线到真实消费者」）。
//
// **为何收敛**：此前 `volumeStatusMeta`（`volumes_api.go`）与 `volumeUsageOf`（`metrics.go`）
// 各持一份**重复**逻辑（「容量池 `Pool.Usage()` + 外部卷 `UsageProvider` 覆盖」）⇒ **多源即多漂移面**
// （改一处漏一处）—— 正是设计 §0 的根因形态（横切关注点实现成 N 个调用点义务）。
//
// **配额层为第一优先级**：经唯一入口 `capacity.WrapStorageVolume` 装配的卷其 `QuotaVolume` 可达
// （`capacity.Backend.Quota()`）⇒ 用量从配额层读（其 counter 与 `CapacityFS` **共享同一实例** ⇒
// 与旧路径同值 ✓，但**查询面统一** ✓，为后续「整卷 + 每用户」查询收敛到一处铺路 ✓）。

import (
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// quotaLayerProvider 是「能暴露配额层」的后端（`*capacity.Backend` 结构性满足 ⇒ **无需 import** ✓）。
type quotaLayerProvider interface {
	Quota() *registry.QuotaVolume
}

// volumeQuotaLayer 返回该卷的配额层（未接线卷 ⇒ nil）。
func (h *Handlers) volumeQuotaLayer(name string) *registry.QuotaVolume {
	be := h.volSet.External(name)
	if be == nil {
		return nil
	}
	if q, ok := be.(quotaLayerProvider); ok {
		return q.Quota()
	}
	return nil
}

// volumeUsage 返回卷当前已用字节（**唯一实现**，供列表/指标等消费）。
//
// 优先级：① **配额层**（`QuotaVolume.VolumeUsed`，其 counter 即卷容量池 ⇒ 与 ② 同值 ✓）；
// ② 容量池 `Pool.Usage()`（本地卷 / 未接线卷）；③ 外部卷后端 `UsageProvider.Usage()`
// （后端真值优先，覆盖前两者 —— 与既有语义一致 ✓）。
func (h *Handlers) volumeUsage(v volume.Volume) int64 {
	var usage int64
	if q := h.volumeQuotaLayer(v.Name); q != nil {
		if used, ok := q.VolumeUsed(); ok {
			usage = used
		}
	} else if p := h.volSet.Pool(v.Name); p != nil {
		usage = p.Usage()
	}
	if v.Type != "" && v.Type != volume.TypeLocal {
		if be := h.volSet.External(v.Name); be != nil {
			if up, ok := be.(registry.UsageProvider); ok {
				usage = up.Usage()
			}
		}
	}
	return usage
}
