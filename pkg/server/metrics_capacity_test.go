// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// metrics_capacity_test.go 钉住容量快照损坏的**指标面**（评审 F5）：计数文件是占用量的唯一
// 权威记录，损坏即 used 归零（限额被放宽一轮）——除启动 Warn 外必须有可告警的 Prometheus 面。

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/capacity"
)

// TestMetrics_VolumeCapacityCorruptCounter /metrics 必须暴露容量快照损坏计数，且能反映
// 真实损坏次数（>0）。
func TestMetrics_VolumeCapacityCorruptCounter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v.capacity")
	if err := os.WriteFile(path, []byte("boom"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, lerr := capacity.Load(path, 1000); lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}

	ts, _ := newTestServerWithMetrics(t)
	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatal(rerr)
	}
	body := string(raw)
	name := "sproxy_volume_capacity_corrupt_total"
	if !strings.Contains(body, name) {
		t.Fatalf("/metrics 应暴露 %s（容量快照损坏可观测面）", name)
	}
	val, ok := metricValue(body, name)
	if !ok {
		t.Fatalf("无法解析 %s 的值", name)
	}
	if val < 1 {
		t.Fatalf("%s = %d，应 >= 1（刚写过一个损坏快照）", name, val)
	}
}

// metricValue 从 Prometheus 文本中取出 `name <value>` 行的值。
func metricValue(body, name string) (int64, bool) {
	for line := range strings.SplitSeq(body, "\n") {
		rest, found := strings.CutPrefix(line, name+" ")
		if !found {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}
