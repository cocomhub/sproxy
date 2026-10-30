// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// metrics_alias.go 是**迁移期兼容层**：指标注册表与渲染已下沉 pkg/server/metrics，
// 装配层 handler 与存量测试用别名保持零改动。
package server

import (
	"strings"

	"github.com/cocomhub/sproxy/pkg/server/metrics"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

type (
	// Metrics 是运行时指标注册表。
	Metrics = metrics.Metrics
	// XferMetrics 是传输层扩展指标快照（WS/QUIC）。
	XferMetrics = metrics.XferMetrics
	// XferConnMetrics 是单传输连接级统计。
	XferConnMetrics = metrics.XferConnMetrics
	// XferMetricsProvider 是传输层扩展指标提供者。
	XferMetricsProvider = metrics.XferMetricsProvider
	// LabeledSample 是带标签的指标样本。
	LabeledSample = metrics.LabeledSample
	// VolumeKey 是卷维度指标键。
	VolumeKey = metrics.VolumeKey
	// rebalanceProgress 是卷再平衡进度状态。
	rebalanceProgress = metrics.RebalanceProgress
)

// NewMetrics 构造指标注册表。
func NewMetrics() *Metrics { return metrics.NewMetrics() }

// newRebalanceProgress 构造再平衡进度状态。
func newRebalanceProgress() *metrics.RebalanceProgress { return metrics.NewRebalanceProgress() }

// 渲染辅助（迁移期转发）。
func writeMetric(b *strings.Builder, name, typ, help string, value int64) {
	metrics.WriteMetric(b, name, typ, help, value)
}
func writeLabeledCounter(b *strings.Builder, name, help string, samples []metrics.LabeledSample) {
	metrics.WriteLabeledCounter(b, name, help, samples)
}
func writeLabeledGauge(b *strings.Builder, name, help string, labels map[string]string, value int64) {
	metrics.WriteLabeledGauge(b, name, help, labels, value)
}
func writeGaugeSamples(b *strings.Builder, name, help string, samples []metrics.LabeledSample) {
	metrics.WriteGaugeSamples(b, name, help, samples)
}
func writeReadLoopBlock(b *strings.Builder, site, desc string, s *mux.BlockStat) {
	metrics.WriteReadLoopBlock(b, site, desc, s)
}
