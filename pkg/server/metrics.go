// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// Metrics 使用 atomic 计数器收集请求统计数据。
// 注意：Go 1.22+ 的 atomic.Int64 自动处理对齐，无需手动对齐。
type Metrics struct {
	RequestsTotal     atomic.Int64
	Requests2XX       atomic.Int64
	Requests4XX       atomic.Int64
	Requests5XX       atomic.Int64
	BytesUploaded     atomic.Int64
	BytesDownloaded   atomic.Int64
	ActiveConnections atomic.Int64
	FilesUploaded     atomic.Int64
	FilesDownloaded   atomic.Int64
	FilesDeleted      atomic.Int64

	// ---- W4：带标签的跨节点指标 ----
	//
	// 用「键 -> 计数」的互斥锁集合而非 sync.Map：这些事件频率很低（每次建链/拒绝一次），
	// 锁竞争可忽略；而 map 让标签导出与渲染简单直白（不需要类型断言）。
	// **基数由配置界定**（节点数、服务数、拒绝原因数都是小集合），不存在 unbounded 增长。
	meshDial          *labeledCounters[meshDialKey]
	meshDialFallback  *labeledCounters[meshFallbackKey]
	remoteWriteDenied *labeledCounters[remoteWriteDeniedKey]

	// ---- 卷健康指标（roadmap §3 P1）：每卷读写延迟/失败率 ----
	// 带 volume+op 标签的请求总数、失败数、累计延迟。基数 = 卷数 × 操作数（小集合）。
	volumeIO         *labeledCounters[volumeIOKey]
	volumeIOFailures *labeledCounters[volumeIOKey]
	volumeIOLatency  *labeledCounters[volumeIOKey]
}

// 带标签指标的键。用具体结构体而非拼接字符串：避免分隔符与标签值冲突（标签值来自配置/对端）。
type (
	meshDialKey struct {
		carrier, node, service, path string
		e2e                          bool
	}
	meshFallbackKey      struct{ node, service string }
	remoteWriteDeniedKey struct{ reason, node string }
	// volumeIOKey 是卷 IO 指标的键：卷名 + 操作（upload/download）。
	volumeIOKey struct{ volume, op string }
)

// labeledCounters 是「键 → 计数」的带标签计数器集合（互斥锁保护）。
//
// labels 在**构造时**绑定：每个指标族自己知道标签怎么排、怎么转义，渲染侧只拿字符串。
type labeledCounters[K comparable] struct {
	mu     sync.Mutex
	m      map[K]int64
	labels func(K) string
}

func newLabeledCounters[K comparable](labels func(K) string) *labeledCounters[K] {
	return &labeledCounters[K]{m: map[K]int64{}, labels: labels}
}

// add 自增一个键（nil 接收者安全：未初始化的 Metrics 不 panic）。
func (c *labeledCounters[K]) add(key K) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[K]int64{}
	}
	c.m[key]++
}

// addN 按 n 累加（延迟总量等场景；nil 接收者安全）。
func (c *labeledCounters[K]) addN(key K, n int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[K]int64{}
	}
	c.m[key] += n
}

// samples 导出全部样本（顺序由渲染侧排序）。
func (c *labeledCounters[K]) samples() []labeledSample {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]labeledSample, 0, len(c.m))
	for k, v := range c.m {
		out = append(out, labeledSample{labels: c.labels(k), value: v})
	}
	return out
}

// NewMetrics 创建并初始化 Metrics。
func NewMetrics() *Metrics {
	return &Metrics{
		meshDial: newLabeledCounters(func(k meshDialKey) string {
			return fmt.Sprintf(`carrier="%s",node="%s",service="%s",path="%s",e2e="%t"`,
				escapeLabel(k.carrier), escapeLabel(k.node), escapeLabel(k.service), escapeLabel(k.path), k.e2e)
		}),
		meshDialFallback: newLabeledCounters(func(k meshFallbackKey) string {
			return fmt.Sprintf(`node="%s",service="%s"`, escapeLabel(k.node), escapeLabel(k.service))
		}),
		remoteWriteDenied: newLabeledCounters(func(k remoteWriteDeniedKey) string {
			return fmt.Sprintf(`node="%s",reason="%s"`, escapeLabel(k.node), escapeLabel(k.reason))
		}),
		volumeIO: newLabeledCounters(func(k volumeIOKey) string {
			return fmt.Sprintf(`volume="%s",op="%s"`, escapeLabel(k.volume), escapeLabel(k.op))
		}),
		volumeIOFailures: newLabeledCounters(func(k volumeIOKey) string {
			return fmt.Sprintf(`volume="%s",op="%s"`, escapeLabel(k.volume), escapeLabel(k.op))
		}),
		volumeIOLatency: newLabeledCounters(func(k volumeIOKey) string {
			return fmt.Sprintf(`volume="%s",op="%s"`, escapeLabel(k.volume), escapeLabel(k.op))
		}),
	}
}

// RecordRequest 根据状态码记录一次请求。
func (m *Metrics) RecordRequest(statusCode int) {
	m.RequestsTotal.Add(1)
	switch {
	case statusCode >= 200 && statusCode < 300:
		m.Requests2XX.Add(1)
	case statusCode >= 400 && statusCode < 500:
		m.Requests4XX.Add(1)
	case statusCode >= 500:
		m.Requests5XX.Add(1)
	}
}

// RecordUpload 记录上传字节数和文件数。
func (m *Metrics) RecordUpload(bytes int64) {
	m.BytesUploaded.Add(bytes)
	m.FilesUploaded.Add(1)
}

// RecordDownload 记录下载字节数和文件数。
func (m *Metrics) RecordDownload(bytes int64) {
	m.BytesDownloaded.Add(bytes)
	m.FilesDownloaded.Add(1)
}

// RecordDelete 记录删除。
func (m *Metrics) RecordDelete() {
	m.FilesDeleted.Add(1)
}

// RecordMeshDial 记一次**成功**的 mesh 建链（W4）：按 `carrier`+目标（node/service）+路径（path）打标签；
// `fellBack` 为真时同时计入「打洞失败后回落中继」计数。
//
// path 是 SmartDial 竞速候选路径（T7）："via-relay"（经中间节点 X 中继）/ "via-direct"（打洞直连 X）/
// 空 = 非多跳（webrtc 直连或 hub 中继单跳）。当前 RemoteDialer 服务同步链路不经 SmartDial（恒空），
// 多跳路径由 CLI 侧 SmartDial 竞速产生——服务端 metrics 预留该维度供未来同步链路接入。
//
// 语义边界：只记**成功**（失败且未回落没有可用链路，记成任何一种载体都是错的）。
func (m *Metrics) RecordMeshDial(carrier, node, service, path string, fellBack bool) {
	m.RecordMeshDialE2E(carrier, node, service, path, fellBack, false)
}

// RecordMeshDialE2E 是 RecordMeshDial 的 E2E 变体：e2e=true 时计入端到端加密
// 建链（安全开关生效可观测——用户红线：E2E 启用状态必须可观测，禁静默降级）。
func (m *Metrics) RecordMeshDialE2E(carrier, node, service, path string, fellBack bool, e2e bool) {
	if m == nil {
		return
	}
	m.meshDial.add(meshDialKey{carrier: carrier, node: node, service: service, path: path, e2e: e2e})
	if fellBack {
		m.meshDialFallback.add(meshFallbackKey{node: node, service: service})
	}
}

// RecordRemoteWriteDenied 记一次跨节点**写面授权拒绝**（W4），按 reason+node 打标签。
//
// 只记授权类拒绝（未认证 / 卷缺失 / 指纹未 pin / scope 不授予写等），**不含**配额超限、校验和不符
// 这类「已授权但业务失败」——它们各有 HTTP 语义，混在一起会让「授权被拒」这个信号失真。
func (m *Metrics) RecordRemoteWriteDenied(reason, node string) {
	if m == nil {
		return
	}
	m.remoteWriteDenied.add(remoteWriteDeniedKey{reason: reason, node: node})
}

// RecordVolumeIO 记一次卷 IO（upload/download）指标（roadmap §3 P1）：请求总数 + 失败数 +
// 累计延迟（纳秒），按 volume+op 打标签。ok=true 记总数不记失败；ok=false 总数+失败都记。
// latency 是该次 IO 耗时（上传/下载处理时长）。
func (m *Metrics) RecordVolumeIO(volume, op string, latency time.Duration, ok bool) {
	if m == nil {
		return
	}
	key := volumeIOKey{volume: volume, op: op}
	m.volumeIO.add(key)
	m.volumeIOLatency.addN(key, latency.Nanoseconds())
	if !ok {
		m.volumeIOFailures.add(key)
	}
}

// volumeIO*Samples 导出卷 IO 指标样本（排序在渲染处做）。
func (m *Metrics) volumeIOSamples() []labeledSample         { return m.volumeIO.samples() }
func (m *Metrics) volumeIOFailuresSamples() []labeledSample { return m.volumeIOFailures.samples() }
func (m *Metrics) volumeIOLatencySamples() []labeledSample  { return m.volumeIOLatency.samples() }

// labeledSample 是一条渲染好的带标签样本（labels 已转义并格式化）。
type labeledSample struct {
	labels string
	value  int64
}

// writeLabeledCounter 写一组带标签计数器：HELP/TYPE 一次 + 每 series 一行。
//
// **即使没有样本也输出 HELP/TYPE**：让指标在抓取端可被发现（否则「一直没数据」与「指标不存在」
// 在面板上无法区分）。样本按标签串排序 ⇒ 输出稳定，便于 diff 与单测。
func writeLabeledCounter(b *strings.Builder, name, help string, samples []labeledSample) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	sort.Slice(samples, func(i, j int) bool { return samples[i].labels < samples[j].labels })
	for _, s := range samples {
		fmt.Fprintf(b, "%s{%s} %d\n", name, s.labels, s.value)
	}
	b.WriteString("\n")
}

// escapeLabel 按 Prometheus 文本格式转义标签值（反斜杠、双引号、换行）。
// 不转义的话，一个含引号的节点名就能产出畸形文本、让整次抓取失败。
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// meshDialSamples / meshDialFallbackSamples / remoteWriteDeniedSamples 导出带标签样本（排序在渲染处做）。
func (m *Metrics) meshDialSamples() []labeledSample { return m.meshDial.samples() }

func (m *Metrics) meshDialFallbackSamples() []labeledSample { return m.meshDialFallback.samples() }

func (m *Metrics) remoteWriteDeniedSamples() []labeledSample { return m.remoteWriteDenied.samples() }

// RecordMeshDial 记录一次 mesh 载体建链（转发到 Metrics；无 metrics 时不 panic）。
func (h *Handlers) RecordMeshDial(carrier, node, service, path string, fellBack bool) {
	h.metrics.RecordMeshDial(carrier, node, service, path, fellBack)
}

// RecordRemoteWriteDenied 记录一次跨节点写面授权拒绝（转发到 Metrics；无 metrics 时不 panic）。
func (h *Handlers) RecordRemoteWriteDenied(reason, node string) {
	h.metrics.RecordRemoteWriteDenied(reason, node)
}

// Snapshot 返回当前所有指标的快照（用于调试和日志输出）。
func (m *Metrics) Snapshot() map[string]int64 {
	if m == nil {
		return nil
	}
	return map[string]int64{
		"requests_total":     m.RequestsTotal.Load(),
		"requests_2xx":       m.Requests2XX.Load(),
		"requests_4xx":       m.Requests4XX.Load(),
		"requests_5xx":       m.Requests5XX.Load(),
		"bytes_uploaded":     m.BytesUploaded.Load(),
		"bytes_downloaded":   m.BytesDownloaded.Load(),
		"active_connections": m.ActiveConnections.Load(),
		"files_uploaded":     m.FilesUploaded.Load(),
		"files_downloaded":   m.FilesDownloaded.Load(),
		"files_deleted":      m.FilesDeleted.Load(),
	}
}

// aggregateMuxMetrics 从 MeshRouteTable 中所有已注册 mux 实例聚合 mux 级指标
// （跨全部 mesh 汇总，/metrics 为运维面，不区分调用方 mesh）。
// 返回 nil 表示没有可用的 mux 实例。
func (h *Handlers) aggregateMuxMetrics() *mux.Metrics {
	if h.routeTable == nil {
		return nil
	}
	var total mux.Metrics
	found := false
	for _, mesh := range h.routeTable.AllMeshes() {
		for _, n := range h.routeTable.List(mesh) {
			if n.Mux == nil {
				continue
			}
			found = true
			mm := n.Mux.Metrics()
			total.Streams.Opened.Add(mm.Streams.Opened.Load())
			total.Streams.BytesRead.Add(mm.Streams.BytesRead.Load())
			total.Streams.BytesWritten.Add(mm.Streams.BytesWritten.Load())
			// Active 是**当前值**（求和）：跨 mux 的活跃流总数 = 各 mux 之和。
			total.Streams.Active.Add(mm.Streams.Active.Load())
			// MaxActive 是**峰值**类（取最大而非求和）：跨 mux 反映任意时刻同时活跃的最大流数。
			if v := mm.Streams.MaxActive.Load(); v > total.Streams.MaxActive.Load() {
				total.Streams.MaxActive.Store(v)
			}
			// LongestIdle 是**峰值**类（取最大）：最久空闲流跨 mux 取最大。
			if v := n.Mux.LongestIdle(); v.Nanoseconds() > total.LongestIdleNanos.Load() {
				total.LongestIdleNanos.Store(v.Nanoseconds())
			}
			total.FramesSent.Add(mm.FramesSent.Load())
			total.FramesReceived.Add(mm.FramesReceived.Load())
			total.PingsSent.Add(mm.PingsSent.Load())
			total.PongsReceived.Add(mm.PongsReceived.Load())
			total.PongsSent.Add(mm.PongsSent.Load())
			total.PongsCoalesced.Add(mm.PongsCoalesced.Load())
			total.PongsDropped.Add(mm.PongsDropped.Load())
			total.DatagramHandlerDrops.Add(mm.DatagramHandlerDrops.Load())
			total.StreamOverflowSpills.Add(mm.StreamOverflowSpills.Load())
			total.StreamWindowViolations.Add(mm.StreamWindowViolations.Load())
			total.Retransmits.Add(mm.Retransmits.Load())
			total.RetransmitQueueFull.Add(mm.RetransmitQueueFull.Load())
			total.RetransmitExhausted.Add(mm.RetransmitExhausted.Load())
			// readLoop 阻塞观测：次数/耗时求和，单次峰值取最大（与单 mux 语义一致）。
			total.ReadLoopPush.MergeFrom(&mm.ReadLoopPush)
			total.ReadLoopDatagram.MergeFrom(&mm.ReadLoopDatagram)
			total.ReadLoopPong.MergeFrom(&mm.ReadLoopPong)
			// dataCh 水位与「已收未消费字节峰值」都是「峰值」类 gauge：跨 mux 取最大而非求和。
			if v := mm.DataChMaxFrames.Load(); v > total.DataChMaxFrames.Load() {
				total.DataChMaxFrames.Store(v)
			}
			if v := mm.MaxBufferedBytes.Load(); v > total.MaxBufferedBytes.Load() {
				total.MaxBufferedBytes.Store(v)
			}
			total.Errors.Add(mm.Errors.Load())
			total.Streams.Errors.Add(mm.Streams.Errors.Load())
		}
	}
	if !found {
		return nil
	}
	return &total
}

// MetricsHandler 返回 GET /metrics 的 HTTP handler。
// 使用 Prometheus 文本格式（仅标准库，无依赖）。
func (h *Handlers) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	m := h.metrics
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if m == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# No metrics collected\n"))
		return
	}

	w.WriteHeader(http.StatusOK)

	var b strings.Builder
	writeMetric(&b, "sproxy_requests_total", "counter", "Total HTTP requests", m.RequestsTotal.Load())
	writeMetric(&b, "sproxy_requests_2xx", "counter", "HTTP 2xx requests", m.Requests2XX.Load())
	writeMetric(&b, "sproxy_requests_4xx", "counter", "HTTP 4xx requests", m.Requests4XX.Load())
	writeMetric(&b, "sproxy_requests_5xx", "counter", "HTTP 5xx requests", m.Requests5XX.Load())
	writeMetric(&b, "sproxy_bytes_uploaded", "counter", "Total bytes uploaded", m.BytesUploaded.Load())
	writeMetric(&b, "sproxy_bytes_downloaded", "counter", "Total bytes downloaded", m.BytesDownloaded.Load())
	writeMetric(&b, "sproxy_active_connections", "gauge", "Currently active connections", m.ActiveConnections.Load())
	writeMetric(&b, "sproxy_files_uploaded", "counter", "Total files uploaded", m.FilesUploaded.Load())
	writeMetric(&b, "sproxy_files_downloaded", "counter", "Total files downloaded", m.FilesDownloaded.Load())
	writeMetric(&b, "sproxy_files_deleted", "counter", "Total files deleted", m.FilesDeleted.Load())

	// Mux 级指标（从 RouteTable 实时聚合）
	if mm := h.aggregateMuxMetrics(); mm != nil {
		writeMetric(&b, "sproxy_mux_streams_opened", "counter", "Mux streams opened", mm.Streams.Opened.Load())
		writeMetric(&b, "sproxy_mux_streams_active", "gauge", "Currently active mux streams (acceptor-side streams are only reaped when the peer closes; a growing count with idle streams hints at a stuck peer)", mm.Streams.Active.Load())
		writeMetric(&b, "sproxy_mux_streams_active_max", "gauge", "Peak concurrent active mux streams", mm.Streams.MaxActive.Load())
		writeMetric(&b, "sproxy_mux_stream_longest_idle_nanos", "gauge", "Longest idle time among currently active mux streams in nanoseconds (growing = suspected leaked stream from a stuck peer)", mm.LongestIdleNanos.Load())
		writeMetric(&b, "sproxy_mux_bytes_read", "counter", "Mux bytes read", mm.Streams.BytesRead.Load())
		writeMetric(&b, "sproxy_mux_bytes_written", "counter", "Mux bytes written", mm.Streams.BytesWritten.Load())
		writeMetric(&b, "sproxy_mux_frames_sent", "counter", "Mux frames sent", mm.FramesSent.Load())
		writeMetric(&b, "sproxy_mux_frames_received", "counter", "Mux frames received", mm.FramesReceived.Load())
		writeMetric(&b, "sproxy_mux_pings_sent", "counter", "Mux pings sent", mm.PingsSent.Load())
		writeMetric(&b, "sproxy_mux_pongs_received", "counter", "Mux pongs received", mm.PongsReceived.Load())
		writeMetric(&b, "sproxy_mux_pongs_sent", "counter", "Mux pongs written in reply to pings", mm.PongsSent.Load())
		writeMetric(&b, "sproxy_mux_pongs_coalesced", "counter", "Ping replies deferred to the ticker because the write queue was full", mm.PongsCoalesced.Load())
		writeMetric(&b, "sproxy_mux_pongs_dropped", "counter", "Ping replies dropped because the send failed (self-healing; not counted as errors)", mm.PongsDropped.Load())
		writeMetric(&b, "sproxy_mux_datagram_handler_drops", "counter", "Datagrams dropped by the registered datagram handler (reported by the handler)", mm.DatagramHandlerDrops.Load())
		writeMetric(&b, "sproxy_mux_stream_datach_max_frames", "gauge", "Max observed per-stream receive channel occupancy in frames (capacity 64; overflow goes to the per-stream overflow buffer)", mm.DataChMaxFrames.Load())
		writeMetric(&b, "sproxy_mux_stream_overflow_spills", "counter", "Frames moved to the per-stream overflow buffer because the receive channel was full (previously blocked the read loop)", mm.StreamOverflowSpills.Load())
		writeMetric(&b, "sproxy_mux_stream_window_violations", "counter", "Streams aborted because the peer exceeded its flow-control window (fail-closed, connection kept alive)", mm.StreamWindowViolations.Load())
		writeMetric(&b, "sproxy_mux_retransmits_total", "counter", "Frames successfully retransmitted after a transient send failure (growing = connection quality degradation)", mm.Retransmits.Load())
		writeMetric(&b, "sproxy_mux_retransmit_queue_full_total", "counter", "Muxes closed because the retransmit queue filled (>=256 pending frames failed to send; connection unusable)", mm.RetransmitQueueFull.Load())
		writeMetric(&b, "sproxy_mux_retransmit_exhausted_total", "counter", "Muxes closed because retransmit retries were exhausted (backoff budget spent; connection unusable)", mm.RetransmitExhausted.Load())
		writeMetric(&b, "sproxy_mux_stream_buffered_max_bytes", "gauge", "Max observed bytes received but not yet consumed by the application for a single stream (window-bounded)", mm.MaxBufferedBytes.Load())
		writeReadLoopBlock(&b, "push", "a stream receive-buffer push (receiver-side backpressure)", &mm.ReadLoopPush)
		writeReadLoopBlock(&b, "datagram", "a synchronous datagram handler call", &mm.ReadLoopDatagram)
		writeReadLoopBlock(&b, "pong", "a ping reply", &mm.ReadLoopPong)
		writeMetric(&b, "sproxy_mux_errors", "counter", "Mux errors", mm.Errors.Load())
		writeMetric(&b, "sproxy_mux_stream_errors", "counter", "Mux stream errors", mm.Streams.Errors.Load())
	}
	// Hub 级指标：按调用方 mesh 统计（无 mesh 请求时汇总所有 mesh）。
	if rt := h.routeTable; rt != nil {
		count := 0
		if mesh := meshFromRequest(r); mesh != "" {
			count = rt.NodeCount(mesh)
		} else {
			for _, m := range rt.AllMeshes() {
				count += rt.NodeCount(m)
			}
		}
		writeMetric(&b, "sproxy_hub_nodes_connected", "gauge", "Current number of connected relay nodes", int64(count))
	}
	// W4：跨节点带标签指标（载体/回落/写面拒绝）。
	writeLabeledCounter(&b, "sproxy_mesh_dial_total", "Successful mesh link establishments by carrier and target", m.meshDialSamples())
	writeLabeledCounter(&b, "sproxy_mesh_dial_fallback_total", "Mesh dials that fell back to relay after a failed direct attempt", m.meshDialFallbackSamples())
	writeLabeledCounter(&b, "sproxy_remote_write_denied_total", "Remote write authorization denials by reason and peer node", m.remoteWriteDeniedSamples())

	// 卷健康指标（roadmap §3 P1）：每卷读写延迟/失败率（volume+op 标签）。
	writeLabeledCounter(&b, "sproxy_volume_io_total", "Per-volume IO requests by operation (upload/download)", m.volumeIOSamples())
	writeLabeledCounter(&b, "sproxy_volume_io_failures_total", "Per-volume IO failures by operation (failure rate = failures / total)", m.volumeIOFailuresSamples())
	writeLabeledCounter(&b, "sproxy_volume_io_latency_nanos_total", "Per-volume cumulative IO latency in nanoseconds by operation", m.volumeIOLatencySamples())

	// 云端下载指标
	if cm := h.cloudMgr; cm != nil && cm.Metrics() != nil {
		cmMetrics := cm.Metrics()
		writeMetric(&b, "sproxy_cloud_tasks_created", "counter", "Total cloud download tasks created", cmMetrics.TasksCreated.Load())
		writeMetric(&b, "sproxy_cloud_tasks_completed", "counter", "Total cloud download tasks completed", cmMetrics.TasksCompleted.Load())
		writeMetric(&b, "sproxy_cloud_tasks_failed", "counter", "Total cloud download tasks failed", cmMetrics.TasksFailed.Load())
		writeMetric(&b, "sproxy_cloud_tasks_cancelled", "counter", "Total cloud download tasks cancelled", cmMetrics.TasksCancelled.Load())
		writeMetric(&b, "sproxy_cloud_bytes_downloaded", "counter", "Total bytes downloaded by cloud downloader", cmMetrics.BytesDownloaded.Load())
		writeMetric(&b, "sproxy_cloud_active_downloads", "gauge", "Currently active cloud downloads", cmMetrics.ActiveDownloads.Load())
	}

	_, _ = w.Write([]byte(b.String()))
}

// writeMetric 写入一个 Prometheus 格式的指标到 strings.Builder。
func writeMetric(b *strings.Builder, name, typ, help string, value int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n%s %d\n\n", name, help, name, typ, name, value)
}

// writeReadLoopBlock 渲染 readLoop 内单条「可能阻塞路径」的观测（次数/累计耗时/单次峰值）。
// 三个指标同族：waits 是进入次数，nanos_total 是累计耗时，nanos_max 是单次峰值——
// 后者用于回答「这条路径到底会不会真的停摆连接」（时间均值会被大量 0 耗时冲淡）。
func writeReadLoopBlock(b *strings.Builder, site, desc string, s *mux.BlockStat) {
	writeMetric(b, "sproxy_mux_readloop_"+site+"_waits", "counter", "Times readLoop entered "+desc, s.Waits.Load())
	writeMetric(b, "sproxy_mux_readloop_"+site+"_nanos_total", "counter", "Total nanoseconds readLoop spent in "+desc, s.Nanos.Load())
	writeMetric(b, "sproxy_mux_readloop_"+site+"_nanos_max", "gauge", "Max nanoseconds readLoop spent in a single "+desc, s.MaxNanos.Load())
}

// metricsResponseWriter 包装 http.ResponseWriter，捕获状态码。
type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	reqActor    string // 已认证操作主体（authMiddleware 写入，供请求日志记录）
}

func newMetricsResponseWriter(w http.ResponseWriter) *metricsResponseWriter {
	return &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

// setActor 记录已认证操作主体（实现 actorCarrier，供 requestLogMiddleware 读取）。
func (mw *metricsResponseWriter) setActor(a string) { mw.reqActor = a }

// actor 返回已认证操作主体（未认证为空串）。
func (mw *metricsResponseWriter) actor() string { return mw.reqActor }

func (mw *metricsResponseWriter) WriteHeader(code int) {
	if !mw.wroteHeader {
		mw.statusCode = code
		mw.wroteHeader = true
		mw.ResponseWriter.WriteHeader(code)
	}
}

func (mw *metricsResponseWriter) Write(b []byte) (int, error) {
	if !mw.wroteHeader {
		mw.WriteHeader(http.StatusOK)
	}
	return mw.ResponseWriter.Write(b)
}

// Hijack 实现 http.Hijacker，委托给底层 ResponseWriter。
func (mw *metricsResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := mw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("metricsResponseWriter: underlying ResponseWriter does not implement http.Hijacker")
}

// Flush 实现 http.Flusher，委托给底层 ResponseWriter。
func (mw *metricsResponseWriter) Flush() {
	if f, ok := mw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// metricsMiddleware 自动记录请求状态码和活跃连接数。
// 在 Handler 链外层使用，捕获所有响应的状态码。
func (h *Handlers) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.metrics == nil {
			next.ServeHTTP(w, r)
			return
		}
		h.metrics.ActiveConnections.Add(1)
		defer h.metrics.ActiveConnections.Add(-1)

		mw := newMetricsResponseWriter(w)
		next.ServeHTTP(mw, r)

		h.metrics.RecordRequest(mw.statusCode)
	})
}
