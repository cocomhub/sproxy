// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// samplesToMap 把样本折成「标签串 → 值」映射：样本顺序由渲染侧排序，
// 这里按标签查值，避免依赖 map 迭代顺序。
func samplesToMap(samples []LabeledSample) map[string]int64 {
	out := make(map[string]int64, len(samples))
	for _, s := range samples {
		out[s.Labels] = s.Value
	}
	return out
}

// assertSamplesEqual 断言样本集合与期望完全一致（数量 + 每条标签值）。
func assertSamplesEqual(t *testing.T, got []LabeledSample, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("样本数 = %d, want %d（got=%v）", len(got), len(want), got)
	}
	gotMap := samplesToMap(got)
	for labels, wantVal := range want {
		gotVal, ok := gotMap[labels]
		if !ok {
			t.Errorf("缺少样本 %s", labels)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("样本 %s = %d, want %d", labels, gotVal, wantVal)
		}
	}
}

func TestLabeledCounters_AddAndSamples(t *testing.T) {
	t.Parallel()

	c := newLabeledCounters(func(k string) string { return `reason="` + k + `"` })
	c.add("beta")
	c.add("alpha")
	c.add("alpha")
	c.addN("gamma", 7)
	c.addN("gamma", 5)

	want := map[string]int64{
		`reason="alpha"`: 2,
		`reason="beta"`:  1,
		`reason="gamma"`: 12,
	}
	assertSamplesEqual(t, c.samples(), want)
}

func TestLabeledCounters_NilReceiverIsNoOp(t *testing.T) {
	t.Parallel()

	var c *labeledCounters[string]
	c.add("alpha")
	c.addN("beta", 3)
	if got := c.samples(); got != nil {
		t.Fatalf("nil 接收者 samples = %v, want nil", got)
	}
}

func TestLabeledCounters_ConcurrentAdd(t *testing.T) {
	t.Parallel()

	const workers, perWorker = 8, 200
	c := newLabeledCounters(strconv.Itoa)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for range perWorker {
				c.add(w % 2)
				c.addN(2, 1)
			}
		}(w)
	}
	wg.Wait()

	got := samplesToMap(c.samples())
	const total = int64(workers * perWorker)
	if got["2"] != total {
		t.Errorf("共享键计数 = %d, want %d（并发累加丢失）", got["2"], total)
	}
	if sum := got["0"] + got["1"]; sum != total {
		t.Errorf("按键求和 = %d, want %d", sum, total)
	}
}

func TestLabeledGauges_SetOverwriteAndDelete(t *testing.T) {
	t.Parallel()

	g := newLabeledGauges(func(k VolumeKey) string { return `volume="` + k.volume + `"` })
	g.set(VolumeKey{volume: "vol-a"}, 10)
	g.set(VolumeKey{volume: "vol-a"}, 42) // 瞬时值：覆盖而非累加
	g.set(VolumeKey{volume: "vol-b"}, 7)
	g.deleteKey(VolumeKey{volume: "vol-b"})
	g.deleteKey(VolumeKey{volume: "never-set"}) // 删除不存在的键不 panic

	assertSamplesEqual(t, g.samples(), map[string]int64{`volume="vol-a"`: 42})
}

func TestLabeledGauges_NilReceiverIsNoOp(t *testing.T) {
	t.Parallel()

	var g *labeledGauges[VolumeKey]
	g.set(VolumeKey{volume: "vol-a"}, 1)
	g.deleteKey(VolumeKey{volume: "vol-a"})
	if got := g.samples(); got != nil {
		t.Fatalf("nil 接收者 samples = %v, want nil", got)
	}
}

func TestMetrics_RecordRequestByStatusClass(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	for _, code := range []int{100, 200, 204, 299, 302, 400, 404, 499, 500, 599} {
		m.RecordRequest(code)
	}

	if got := m.RequestsTotal.Load(); got != 10 {
		t.Errorf("requests_total = %d, want 10", got)
	}
	if got := m.Requests2XX.Load(); got != 3 {
		t.Errorf("requests_2xx = %d, want 3（200/204/299）", got)
	}
	if got := m.Requests4XX.Load(); got != 3 {
		t.Errorf("requests_4xx = %d, want 3（400/404/499）", got)
	}
	if got := m.Requests5XX.Load(); got != 2 {
		t.Errorf("requests_5xx = %d, want 2（500/599）", got)
	}
}

func TestMetrics_RecordUploadDownloadDeleteAndSnapshot(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordUpload(100)
	m.RecordUpload(50)
	m.RecordDownload(300)
	m.RecordDelete()

	want := map[string]int64{
		"requests_total":     0,
		"requests_2xx":       0,
		"requests_4xx":       0,
		"requests_5xx":       0,
		"bytes_uploaded":     150,
		"bytes_downloaded":   300,
		"active_connections": 0,
		"files_uploaded":     2,
		"files_downloaded":   1,
		"files_deleted":      1,
	}
	snapshot := m.Snapshot()
	if len(snapshot) != len(want) {
		t.Fatalf("Snapshot 键数 = %d, want %d", len(snapshot), len(want))
	}
	for k, wantVal := range want {
		if got := snapshot[k]; got != wantVal {
			t.Errorf("Snapshot[%q] = %d, want %d", k, got, wantVal)
		}
	}
}

func TestMetrics_SnapshotNilReceiver(t *testing.T) {
	t.Parallel()

	var m *Metrics
	if got := m.Snapshot(); got != nil {
		t.Fatalf("nil 接收者 Snapshot = %v, want nil", got)
	}
}

func TestMetrics_RecordKeyframeFragmented(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordKeyframeFragmented()
	m.RecordKeyframeFragmented()
	if got := m.KeyframeFragmented.Load(); got != 2 {
		t.Errorf("keyframe_fragmented = %d, want 2", got)
	}

	var nilMetrics *Metrics
	nilMetrics.RecordKeyframeFragmented() // 未装配 metrics 时 no-op
}

func TestMetrics_RecordVolumeIO(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordVolumeIO("vol-a", "upload", 30*time.Millisecond, true)
	m.RecordVolumeIO("vol-a", "upload", 20*time.Millisecond, false)
	m.RecordVolumeIO("vol-a", "download", time.Millisecond, true)

	assertSamplesEqual(t, m.VolumeIOSamples(), map[string]int64{
		`volume="vol-a",op="upload"`:   2,
		`volume="vol-a",op="download"`: 1,
	})
	assertSamplesEqual(t, m.VolumeIOFailuresSamples(), map[string]int64{
		`volume="vol-a",op="upload"`: 1,
	})
	assertSamplesEqual(t, m.VolumeIOLatencySamples(), map[string]int64{
		`volume="vol-a",op="upload"`:   (50 * time.Millisecond).Nanoseconds(),
		`volume="vol-a",op="download"`: time.Millisecond.Nanoseconds(),
	})

	var nilMetrics *Metrics
	nilMetrics.RecordVolumeIO("vol-a", "upload", time.Millisecond, false)
}

func TestMetrics_SetVolumeUsageAndCapacitySamples(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.SetVolumeUsage("vol-a", 100, 1000)
	m.SetVolumeUsage("vol-b", 5, 0) // 无限容量：只记已用，不输出容量序列

	assertSamplesEqual(t, m.VolumeUsageSamples(), map[string]int64{
		`volume="vol-a"`: 100,
		`volume="vol-b"`: 5,
	})
	assertSamplesEqual(t, m.VolumeCapSamples(), map[string]int64{
		`volume="vol-a"`: 1000,
	})

	// 卷改为无限容量 ⇒ 清掉残留容量样本（避免告警按陈旧上限计算）。
	m.SetVolumeUsage("vol-a", 200, 0)
	assertSamplesEqual(t, m.VolumeCapSamples(), map[string]int64{})

	var nilMetrics *Metrics
	nilMetrics.SetVolumeUsage("vol-a", 1, 1)
}

func TestMetrics_RecordBackupTask(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordBackupTask()
	m.RecordBackupTask()
	m.RecordBackupTaskFailed()
	if got := m.BackupTasks(); got != 2 {
		t.Errorf("backup_tasks = %d, want 2", got)
	}
	if got := m.BackupTasksFailed(); got != 1 {
		t.Errorf("backup_tasks_failed = %d, want 1", got)
	}

	var nilMetrics *Metrics
	nilMetrics.RecordBackupTask()
	nilMetrics.RecordBackupTaskFailed()
}

func TestMetrics_RecordRateLimitRejected(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordRateLimitRejected("ip", "/upload")
	m.RecordRateLimitRejected("ip", "/upload")
	m.RecordRateLimitRejected("global", "")

	assertSamplesEqual(t, m.RateLimitRejectedSamples(), map[string]int64{
		`scope="ip",endpoint="/upload"`: 2,
		`scope="global",endpoint=""`:    1,
	})

	var nilMetrics *Metrics
	nilMetrics.RecordRateLimitRejected("ip", "/upload")
	if got := nilMetrics.RateLimitRejectedSamples(); got != nil {
		t.Fatalf("nil 接收者 samples = %v, want nil", got)
	}
}

func TestMetrics_RecordMeshDial(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordMeshDial("ws", "node-a", "svc-1", "via-relay", true)
	m.RecordMeshDialE2E("quic", "node-b", "svc-2", "", false, true)

	assertSamplesEqual(t, m.MeshDialSamples(), map[string]int64{
		`carrier="ws",node="node-a",service="svc-1",path="via-relay",e2e="false"`: 1,
		`carrier="quic",node="node-b",service="svc-2",path="",e2e="true"`:         1,
	})
	assertSamplesEqual(t, m.MeshDialFallbackSamples(), map[string]int64{
		`node="node-a",service="svc-1"`: 1,
	})

	var nilMetrics *Metrics
	nilMetrics.RecordMeshDial("ws", "node-a", "svc-1", "", true)
}

func TestMetrics_RecordRemoteWriteDenied(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordRemoteWriteDenied("no-scope", "node-a")
	m.RecordRemoteWriteDenied("no-scope", "node-a")
	m.RecordRemoteWriteDenied("no-pin", "node-b")

	assertSamplesEqual(t, m.RemoteWriteDeniedSamples(), map[string]int64{
		`node="node-a",reason="no-scope"`: 2,
		`node="node-b",reason="no-pin"`:   1,
	})

	var nilMetrics *Metrics
	nilMetrics.RecordRemoteWriteDenied("no-scope", "node-a")
}

func TestMetrics_RecordAuditRotate(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordAuditRotate("size")
	m.RecordAuditRotate("")
	m.RecordAuditRotate("")

	assertSamplesEqual(t, m.AuditRotateSamples(), map[string]int64{
		`reason="size"`:    1,
		`reason="unknown"`: 2,
	})
}

func TestMetrics_UsageForOwner(t *testing.T) {
	t.Parallel()

	type usageCall struct {
		owner, kind string
		n           int64
	}
	var calls []usageCall

	m := NewMetrics()
	m.SetUsageRecorder(func(owner, kind string, n int64) {
		calls = append(calls, usageCall{owner: owner, kind: kind, n: n})
	})
	m.RecordUploadForOwner("alice", 100)
	m.RecordUploadForOwner("alice", 50)
	m.RecordDownloadForOwner("alice", 30)
	m.RecordUploadForOwner("", 999)   // 未认证请求：跳过
	m.RecordUploadForOwner("bob", 0)  // 非正字节：跳过
	m.RecordUploadForOwner("bob", -5) // 非正字节：跳过
	m.RecordDownloadForOwner("", 100) // 未认证请求：跳过

	assertSamplesEqual(t, m.UsageUploadSamples(), map[string]int64{`owner="alice"`: 150})
	assertSamplesEqual(t, m.UsageDownloadSamples(), map[string]int64{`owner="alice"`: 30})

	want := []usageCall{
		{owner: "alice", kind: "upload_bytes", n: 100},
		{owner: "alice", kind: "upload_bytes", n: 50},
		{owner: "alice", kind: "download_bytes", n: 30},
	}
	if len(calls) != len(want) {
		t.Fatalf("usageRecorder 调用 %d 次, want %d（calls=%v）", len(calls), len(want), calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("calls[%d] = %+v, want %+v", i, calls[i], want[i])
		}
	}

	// nil hook = 仅 Prometheus 面计数（不转发），零回归。
	m.SetUsageRecorder(nil)
	m.RecordUploadForOwner("carol", 10)
	assertSamplesEqual(t, m.UsageUploadSamples(), map[string]int64{
		`owner="alice"`: 150,
		`owner="carol"`: 10,
	})

	var nilMetrics *Metrics
	nilMetrics.RecordUploadForOwner("alice", 1)
	nilMetrics.RecordDownloadForOwner("alice", 1)
	nilMetrics.SetUsageRecorder(func(string, string, int64) {})
}

func TestMetrics_RequestLatencyAccessors(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.RecordRequestLatency(10 * time.Millisecond)
	if got := m.RequestDurationCount(); got != 1 {
		t.Errorf("RequestDurationCount = %d, want 1", got)
	}
	if sat, tol, fru := m.ApdexCounts(); sat != 1 || tol != 0 || fru != 0 {
		t.Errorf("ApdexCounts = (%d,%d,%d), want (1,0,0)", sat, tol, fru)
	}

	// 非正阈值忽略（禁静默降级）：仍按默认 500ms 分档，400ms 属 satisfied。
	m.SetApdexThreshold(0)
	m.SetApdexThreshold(-time.Second)
	m.RecordRequestLatency(400 * time.Millisecond)
	if sat, _, _ := m.ApdexCounts(); sat != 2 {
		t.Errorf("satisfied = %d, want 2（非正阈值应被忽略）", sat)
	}

	// 零值 Metrics（未装配直方图）：记录为 no-op，不 panic。
	zero := &Metrics{}
	zero.RecordRequestLatency(time.Second)
	if got := zero.RequestDurationCount(); got != 0 {
		t.Errorf("零值 Metrics RequestDurationCount = %d, want 0", got)
	}
	if sat, tol, fru := zero.ApdexCounts(); sat != 0 || tol != 0 || fru != 0 {
		t.Errorf("零值 Metrics ApdexCounts = (%d,%d,%d), want 全 0", sat, tol, fru)
	}

	var nilMetrics *Metrics
	nilMetrics.RecordRequestLatency(time.Second)
	nilMetrics.SetApdexThreshold(time.Second)
	if got := nilMetrics.RequestDurationCount(); got != 0 {
		t.Errorf("nil 接收者 RequestDurationCount = %d, want 0", got)
	}
	if sat, tol, fru := nilMetrics.ApdexCounts(); sat != 0 || tol != 0 || fru != 0 {
		t.Errorf("nil 接收者 ApdexCounts = (%d,%d,%d), want 全 0", sat, tol, fru)
	}
}
