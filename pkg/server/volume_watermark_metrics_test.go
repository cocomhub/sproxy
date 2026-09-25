// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_watermark_metrics_test.go 钉住 Prometheus 告警模板消费的缺口指标
// （roadmap 11.5-⑪）：
//   - sproxy_storage_usage_bytes{volume} / sproxy_storage_capacity_bytes{volume}
//     （卷磁盘水位 gauge，配合 SproxyStorageUsageHigh 规则）；
//   - sproxy_backup_tasks_total / sproxy_backup_tasks_failed_total
//     （备份/同步任务计数，配合 SproxyBackupSyncFailures 规则）。
//
// 断言作用在 Prometheus 文本输出上（用户可见契约）。

import (
	"strings"
	"testing"
)

// TestVolumeWatermarkMetrics_SetVolumeUsage 验证 SetVolumeUsage 注入水位后
// /metrics 输出 usage/capacity gauge（volume 标签）。
func TestVolumeWatermarkMetrics_SetVolumeUsage(t *testing.T) {
	t.Parallel()
	ts, h := newTestServerWithMetrics(t)

	h.metrics.SetVolumeUsage("main", 850, 1000)
	h.metrics.SetVolumeUsage("disk2", 100, 0) // 0 容量 = 无限，不输出容量序列

	body := metricsBody(t, ts)
	for _, want := range []string{
		`# TYPE sproxy_storage_usage_bytes gauge`,
		`sproxy_storage_usage_bytes{volume="main"} 850`,
		`sproxy_storage_capacity_bytes{volume="main"} 1000`,
		`sproxy_storage_usage_bytes{volume="disk2"} 100`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q:\n%s", want, body)
		}
	}
	// 容量 0（无限）不输出容量序列（避免 clamp_min 假象）。
	if strings.Contains(body, `sproxy_storage_capacity_bytes{volume="disk2"}`) {
		t.Errorf("容量 0 不应输出 capacity 序列:\n%s", body)
	}
}

// TestVolumeWatermarkMetrics_BackupTaskCounters 验证备份/同步任务计数输出。
func TestVolumeWatermarkMetrics_BackupTaskCounters(t *testing.T) {
	t.Parallel()
	ts, h := newTestServerWithMetrics(t)

	h.metrics.RecordBackupTask()
	h.metrics.RecordBackupTask()
	h.metrics.RecordBackupTaskFailed()

	body := metricsBody(t, ts)
	for _, want := range []string{
		`# TYPE sproxy_backup_tasks_total counter`,
		`sproxy_backup_tasks_total 2`,
		`sproxy_backup_tasks_failed_total 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q:\n%s", want, body)
		}
	}
}

// TestVolumeWatermarkMetrics_NilSafe 钉住 nil metrics 上的设置方法不 panic
// （测试/旧装配路径）。
func TestVolumeWatermarkMetrics_NilSafe(t *testing.T) {
	t.Parallel()
	var m *Metrics
	m.SetVolumeUsage("v", 1, 2)
	m.RecordBackupTask()
	m.RecordBackupTaskFailed()
}
