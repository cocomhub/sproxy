// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// quota.go —— 配额查询**联邦化**的 SDK 侧（片 5 读侧，设计
// docs/designs/2026-10-16-quota-volume-layer.md §1.5）。
//
// **口径**（用户裁定 2026-10-16）：用户配额**按卷持有**；**用户视图 = 对「该用户可访问的卷」
// 求和** ⇒ 服务端 `GET /api/quota` 返回跨卷汇总 + 逐卷明细（本文件是其 SDK 镜像）。
//
// **与 `GetQuota`（`/api/stats` 的 `quota` 段）的关系** ✗：后者是**旧集中树**的配额水位
// （`usage`/`max_bytes`/`watermark`）—— 仍在用但**不是**每卷权威 ⇒ 本联邦视图为**权威源** ✓，
// 集中树仅作**补充**信息（预警水位）。

import (
	"context"
	"fmt"
	"net/http"
)

// VolumeQuota 是**单卷**的用户配额视图（对齐 `pkg/server/quota_federated.go` 的 JSON）。
type VolumeQuota struct {
	Volume string `json:"volume"`
	Kind   string `json:"kind,omitempty"`
	// Used 是本卷上该 owner 的已用字节；`HasQuota=false` 时**语义为未知**（值恒 0）。
	Used int64 `json:"used"`
	// Capacity 是本卷上该 owner 的上限；**0 = 不限**（不是「零配额」）。
	Capacity int64 `json:"capacity"`
	// HasQuota=false ⇒ 本卷**无每用户权威**（未装配配额层 / 该 owner 未登记）⇒ used/capacity
	// **未知**（消费方禁当 0 ✗）。
	HasQuota bool `json:"has_quota"`
}

// FederatedQuota 是 owner 的**跨卷汇总**配额视图。
type FederatedQuota struct {
	Owner string `json:"owner"`
	// Used = Σ 有权威卷的用量（无权威的卷**不计入** ⇒ 见 Partial）。
	Used int64 `json:"used"`
	// Capacity = Σ 有权威且**有上限**的卷的上限（不含不限的卷 ⇒ 见 Unlimited）。
	Capacity int64 `json:"capacity"`
	// Unlimited=true ⇒ 至少一个可访问卷对该 owner **不限**（上限 0）。
	Unlimited bool `json:"unlimited"`
	// Partial=true ⇒ 至少一个**可访问**卷无每用户权威 ⇒ Used/Capacity **不完整**（未知部分
	// 未计入）⇒ 消费方必须显式处置（禁当完整值 ✗）。
	Partial bool `json:"partial"`
	// Volumes 是逐卷明细（诊断用：可定位是哪个卷贡献了用量/上限）。
	Volumes []VolumeQuota `json:"volumes"`
}

// GetFederatedQuota 查询本 owner 的跨卷配额视图（`GET /api/quota`）。
//
// 服务端错误**不吞** ✓（5xx 返回错误 ⇒ 禁静默零值 ✗）。
func (c *FileClient) GetFederatedQuota(ctx context.Context) (*FederatedQuota, error) {
	var out FederatedQuota
	if err := c.doJSON(ctx, http.MethodGet, "/api/quota", nil, &out); err != nil {
		return nil, fmt.Errorf("获取联邦配额: %w", err)
	}
	return &out, nil
}
