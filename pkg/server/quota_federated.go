// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_federated.go —— 配额查询**联邦化**（片 5，设计 docs/designs/2026-10-16-quota-volume-layer.md §1.5）。
//
// **口径**（用户裁定 2026-10-16）：用户配额**按卷持有**（在各卷的配额层 ✓）；**用户视图 =
// 对「该用户可访问的卷」求和** ✓ —— 本文件是**唯一实现**（逐卷读取 + 汇总 ✓）。
//
// 三条不可省的性质 ✓：
//  1. **ACL 收敛**：只遍历 `volume.AllowedVolumes(owner)` ⇒ 不可访问的卷**既不出现也不计入**
//     （否则等于向用户泄露他人卷用量 = doxxing ✗）；
//  2. **未知显式**（禁当 0 ✗）：无每用户权威的卷 `HasQuota=false`，且整体 `Partial=true`
//     —— 与 `QuotaVolume.UserUsage` 的 `ok=false` 语义同型 ✓；
//  3. **上限 0 = 不限**：任一卷不限 ⇒ 总量 `Unlimited=true`（**不得**把 0 当普通值相加 ✗）。

import (
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// volumeQuotaView 是**单卷**的用户配额视图（诊断用：可定位是哪个卷贡献了用量/上限）。
type volumeQuotaView struct {
	Volume string `json:"volume"`
	Kind   string `json:"kind,omitempty"`
	// Used 是本卷上该 owner 的已用字节；`HasQuota=false` 时**语义为未知**（值恒 0）。
	Used int64 `json:"used"`
	// Capacity 是本卷上该 owner 的上限；**0 = 不限**（不是「零配额」✗）。
	Capacity int64 `json:"capacity"`
	// HasQuota=false ⇒ 本卷**无每用户权威**（未装配配额层 / 该 owner 未登记）⇒ used/capacity
	// **未知**（禁当 0 消费 ✗）。
	HasQuota bool `json:"has_quota"`
}

// quotaWaitView 是**等待队列可观测**快照（S4）：跨卷汇总 `pkg/waitq` 统计。
// 语义：只统计**有配额池**的可访问卷（`volumes` 为参与卷数）；`volumes=0` ⇒ 无观测面。
type quotaWaitView struct {
	Volumes     int    `json:"volumes"`
	Waiting     int    `json:"waiting"`     // 当前等待者
	MaxWaiting  int    `json:"max_waiting"` // 历史峰值
	Acquired    uint64 `json:"acquired"`    // 成功获取次数
	TimedOut    uint64 `json:"timed_out"`   // 超时次数
	Canceled    uint64 `json:"canceled"`    // 取消次数
	QueueFull   uint64 `json:"queue_full"`  // 背压拒绝（队列已满 ⇒ 429）
	TooLarge    uint64 `json:"too_large"`   // 不可满足（超容量 ⇒ 不排队）
	TotalWaitMS int64  `json:"total_wait_ms"`
}

// federatedQuotaResponse 是 owner 的**跨卷汇总**视图。
type federatedQuotaResponse struct {
	Owner string `json:"owner"`
	// Used = Σ 有权威卷的用量（无权威的卷**不计入** ⇒ 见 Partial）。
	Used int64 `json:"used"`
	// Capacity = Σ 有权威且**有上限**的卷的上限（不含不限的卷 ⇒ 见 Unlimited）。
	Capacity int64 `json:"capacity"`
	// Unlimited=true ⇒ 至少一个可访问卷对该 owner **不限**（上限 0）⇒ 总量不限。
	Unlimited bool `json:"unlimited"`
	// Partial=true ⇒ 至少一个**可访问**卷无每用户权威（或读上限失败）⇒ Used/Capacity
	// **不完整**（未知部分未计入）。消费方必须显式处置（禁当完整值 ✗）。
	Partial bool              `json:"partial"`
	Volumes []volumeQuotaView `json:"volumes"`
	// Wait 是等待队列可观测快照（S4；跨可访问卷汇总）。
	Wait quotaWaitView `json:"wait"`
}

// addWaitStats 把单卷的等待队列统计累加进跨卷视图（S4 等待可观测）。
// 拆成 helper 只为把 federatedUserQuota 的认知复杂度留在阈值内（判定语义不变）。
func addWaitStats(dst *quotaWaitView, q *registry.QuotaVolume) {
	ws, ok := q.WaitStats()
	if !ok {
		return // 该卷无配额池 ⇒ 无观测面（不计入卷数，不伪造零等待）
	}
	dst.Volumes++
	dst.Waiting += ws.Waiting
	if ws.MaxWaiting > dst.MaxWaiting {
		dst.MaxWaiting = ws.MaxWaiting
	}
	dst.Acquired += ws.Acquired
	dst.TimedOut += ws.TimedOut
	dst.Canceled += ws.Canceled
	dst.QueueFull += ws.QueueFull
	dst.TooLarge += ws.TooLarge
	dst.TotalWaitMS += int64(ws.TotalWait / time.Millisecond)
}

// userQuotaHandler 返回**认证用户自己**的跨卷配额视图（`GET /api/quota`）。
//
// owner **恒取自请求认证**（`ownerFromRequest` ⇒ 经 `normalizeOwner` ✓）—— **不接受**请求参数
// 指定 owner（否则等于允许任意用户窥探他人配额 ✗）。
func (h *Handlers) userQuotaHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	sendJSONResponse(w, h.federatedUserQuota(owner), http.StatusOK)
}

// federatedUserQuota 汇总 owner 在**所有可访问卷**上的每用户配额（片 5 的**唯一**汇总入口）。
//
// `owner` 须为**归一化**后的 owner（调用方经 `normalizeOwner` ✓ —— 本层不再归一，避免双口径）。
func (h *Handlers) federatedUserQuota(owner string) federatedQuotaResponse {
	resp := federatedQuotaResponse{Owner: owner, Volumes: []volumeQuotaView{}}
	if h.volSet == nil {
		return resp
	}
	for _, v := range volume.AllowedVolumes(h.volSet.All(), owner) {
		view := volumeQuotaView{Volume: v.Name, Kind: string(registry.KindOf(v))}
		q := h.volumeQuotaLayer(v.Name)
		if q == nil {
			// 该卷无配额层 ⇒ 无每用户权威（**未知**，不是 0 ✗）。
			resp.Partial = true
			resp.Volumes = append(resp.Volumes, view)
			continue
		}
		addWaitStats(&resp.Wait, q)
		used, ok := q.UserUsage(owner)
		if !ok {
			resp.Partial = true
			resp.Volumes = append(resp.Volumes, view)
			continue
		}
		view.HasQuota = true
		view.Used = used
		resp.Used += used

		capBytes, cok := q.UserCapacity(owner)
		if !cok {
			// 用量有权威但上限读不到 ⇒ 数值不完整（保守标记 Partial ✗ 禁静默）。
			resp.Partial = true
		} else {
			view.Capacity = capBytes
			if capBytes == 0 {
				resp.Unlimited = true // 0 = 不限（不参与求和 ✗）
			} else {
				resp.Capacity += capBytes
			}
		}
		resp.Volumes = append(resp.Volumes, view)
	}
	return resp
}
