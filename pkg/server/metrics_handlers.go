// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/metrics"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
)

// RecordMeshDial 记录一次 mesh 载体建链（转发到 Metrics；无 metrics 时不 panic）。
func (h *Handlers) RecordMeshDial(carrier, node, service, path string, fellBack bool) {
	h.metrics.RecordMeshDial(carrier, node, service, path, fellBack)
}

// RecordRemoteWriteDenied 记录一次跨节点写面授权拒绝（转发到 Metrics；无 metrics 时不 panic）。
func (h *Handlers) RecordRemoteWriteDenied(reason, node string) {
	h.metrics.RecordRemoteWriteDenied(reason, node)
}

// writeHubNodeMetrics 输出 per-node 链路质量指标（roadmap P2 节点级状态仪表）：
// sproxy_hub_node_quality{node}（0/1/2 分档）+ sproxy_hub_node_retransmits{node} +
// sproxy_hub_node_errors{node} + sproxy_hub_node_connected_seconds{node}。
func (h *Handlers) writeHubNodeMetrics(b *strings.Builder, r *http.Request) {
	mesh := meshFromRequest(r)
	for _, n := range hubNodesForRequest(h.routeTable, mesh) {
		writeHubNodeGauge(b, h, n, mesh)
	}
}

// hubNodesForRequest 返回请求涉及的节点集合：指定 mesh 时列该 mesh；否则跨全部
// mesh 按节点 ID 去重汇总。
func hubNodesForRequest(rt *hub.MeshRouteTable, mesh string) []hub.NodeInfo {
	if mesh != "" {
		return rt.List(mesh)
	}
	seen := map[hub.NodeID]bool{}
	var nodes []hub.NodeInfo
	for _, m := range rt.AllMeshes() {
		for _, n := range rt.List(m) {
			if !seen[n.ID] {
				seen[n.ID] = true
				nodes = append(nodes, n)
			}
		}
	}
	return nodes
}

// writeHubNodeGauge 输出单个节点的质量分档与链路指标明细（质量/重传/错误/连接时长/RTT）。
func writeHubNodeGauge(b *strings.Builder, h *Handlers, n hub.NodeInfo, mesh string) {
	node := string(n.ID)
	q := qualityRank(h.nodeQuality(node, mesh))
	writeLabeledGauge(b, "sproxy_hub_node_quality", "Per-node link quality tier (0=healthy 1=degraded 2=stale)", map[string]string{"node": node}, int64(q))
	metrics.WriteHubNodeLinkGauge(b, n, node)
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
			metrics.AccumulateNodeMuxMetrics(&total, n)
		}
	}
	if !found {
		return nil
	}
	return &total
}

// renderVolumeWatermarks 把每卷已用/容量水位注入指标并输出（装配层版：卷集合来自 Handlers）。
func (h *Handlers) renderVolumeWatermarks(b *strings.Builder, m *Metrics) {
	if h != nil && h.volSet != nil {
		for _, v := range h.volSet.All() {
			m.SetVolumeUsage(v.Name, h.volumeUsage(v), v.Capacity)
		}
	}
	metrics.WriteGaugeSamples(b, "sproxy_storage_usage_bytes", "Per-volume used bytes (disk watermark)", m.VolumeUsageSamples())
	metrics.WriteGaugeSamples(b, "sproxy_storage_capacity_bytes", "Per-volume capacity bytes (0/absent = unlimited)", m.VolumeCapSamples())
}

// MetricsHandler 返回 GET /metrics 的 HTTP handler。
// 使用 Prometheus 文本格式（仅标准库，无依赖）。
func (h *Handlers) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	m := h.metrics
	w.Header().Set(headerContentType, "text/plain; charset=utf-8")
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
	m.RenderSLOMetrics(&b)
	writeMetric(&b, "sproxy_bytes_uploaded", "counter", "Total bytes uploaded", m.BytesUploaded.Load())
	writeMetric(&b, "sproxy_bytes_downloaded", "counter", "Total bytes downloaded", m.BytesDownloaded.Load())
	writeMetric(&b, "sproxy_active_connections", "gauge", "Currently active connections", m.ActiveConnections.Load())
	writeMetric(&b, "sproxy_files_uploaded", "counter", "Total files uploaded", m.FilesUploaded.Load())
	writeMetric(&b, "sproxy_files_downloaded", "counter", "Total files downloaded", m.FilesDownloaded.Load())
	writeMetric(&b, "sproxy_files_deleted", "counter", "Total files deleted", m.FilesDeleted.Load())
	writeMetric(&b, "sproxy_audit_dropped_total", "counter", "Audit entries dropped WITHOUT being persisted (real loss)", m.AuditDropped.Load())
	writeMetric(&b, "sproxy_audit_evicted_total", "counter", "Audit entries evicted from the hot index after being persisted (not a loss)", m.AuditEvicted.Load())
	writeMetric(&b, "sproxy_audit_write_failed_total", "counter", "Audit journal write failures", m.AuditWriteFailed.Load())
	writeMetric(&b, "sproxy_audit_hook_panics_total", "counter", "Audit downstream hook panics (isolated)", m.AuditHookPanics.Load())
	writeLabeledCounter(&b, "sproxy_audit_rotate_total", "Audit segment rotations by reason", m.AuditRotateSamples())
	writeMetric(&b, "sproxy_keyframe_fmp4_total", "counter", "Uploads detected as fragmented MP4 (fMP4, no global stss) — decision input for mp4ff switch", m.KeyframeFragmented.Load())

	// 传输层连接级指标（xfer TCP：FromNetConn 包装的连接消息/字节计数）。
	tm := builtin.Metrics()
	// WS/QUIC 传输级指标（roadmap 6.x P1 残余）：装配层注入的 provider
	// （cmd/sproxy 经 go.work import ext/ws + ext/quic）；nil = 不输出。
	var wm, qm XferConnMetrics
	if h.xferMetrics != nil {
		xm := h.xferMetrics.XferMetrics()
		wm = xm.WS
		qm = xm.QUIC
	}
	writeMetric(&b, "sproxy_xfer_tcp_conns_opened_total", "counter", "TCP xfer connections opened (FromNetConn wraps)", tm.ConnsOpened)
	writeMetric(&b, "sproxy_xfer_tcp_conns_closed_total", "counter", "TCP xfer connections closed", tm.ConnsClosed)
	writeMetric(&b, "sproxy_xfer_tcp_messages_sent_total", "counter", "TCP xfer messages sent", tm.MessagesSent)
	writeMetric(&b, "sproxy_xfer_tcp_messages_recv_total", "counter", "TCP xfer messages received", tm.MessagesRecv)
	writeMetric(&b, "sproxy_xfer_tcp_bytes_sent_total", "counter", "TCP xfer payload bytes sent", tm.BytesSent)
	writeMetric(&b, "sproxy_xfer_tcp_bytes_recv_total", "counter", "TCP xfer payload bytes received", tm.BytesRecv)
	writeMetric(&b, "sproxy_xfer_ws_conns_opened_total", "counter", "WS xfer connections opened", wm.ConnsOpened)
	writeMetric(&b, "sproxy_xfer_ws_conns_closed_total", "counter", "WS xfer connections closed", wm.ConnsClosed)
	writeMetric(&b, "sproxy_xfer_ws_messages_sent_total", "counter", "WS xfer messages sent", wm.MessagesSent)
	writeMetric(&b, "sproxy_xfer_ws_messages_recv_total", "counter", "WS xfer messages received", wm.MessagesRecv)
	writeMetric(&b, "sproxy_xfer_ws_bytes_sent_total", "counter", "WS xfer payload bytes sent", wm.BytesSent)
	writeMetric(&b, "sproxy_xfer_ws_bytes_recv_total", "counter", "WS xfer payload bytes received", wm.BytesRecv)
	writeMetric(&b, "sproxy_xfer_quic_conns_opened_total", "counter", "QUIC xfer connections opened", qm.ConnsOpened)
	writeMetric(&b, "sproxy_xfer_quic_conns_closed_total", "counter", "QUIC xfer connections closed", qm.ConnsClosed)
	writeMetric(&b, "sproxy_xfer_quic_messages_sent_total", "counter", "QUIC xfer messages sent", qm.MessagesSent)
	writeMetric(&b, "sproxy_xfer_quic_messages_recv_total", "counter", "QUIC xfer messages received", qm.MessagesRecv)
	writeMetric(&b, "sproxy_xfer_quic_bytes_sent_total", "counter", "QUIC xfer payload bytes sent", qm.BytesSent)
	writeMetric(&b, "sproxy_xfer_quic_bytes_recv_total", "counter", "QUIC xfer payload bytes received", qm.BytesRecv)

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
		// 节点级状态仪表（roadmap P2）：per-node 质量分档 + 链路指标明细。
		// 质量分档：0=healthy 1=degraded 2=stale（与 /api/hub/services 同语义）。
		h.writeHubNodeMetrics(&b, r)
	}
	// W4：跨节点带标签指标（载体/回落/写面拒绝）。
	writeLabeledCounter(&b, "sproxy_mesh_dial_total", "Successful mesh link establishments by carrier and target", m.MeshDialSamples())
	writeLabeledCounter(&b, "sproxy_mesh_dial_fallback_total", "Mesh dials that fell back to relay after a failed direct attempt", m.MeshDialFallbackSamples())
	writeLabeledCounter(&b, "sproxy_remote_write_denied_total", "Remote write authorization denials by reason and peer node", m.RemoteWriteDeniedSamples())
	// 限流拒绝计数（roadmap 12.1-6 片 2）：scope=ip/endpoint/global/concurrent。
	writeLabeledCounter(&b, "sproxy_rate_limit_rejected_total", "Rate limit rejections by scope and request path", m.RateLimitRejectedSamples())

	// 计量报告（roadmap 11.10-⑩）owner 维度：per-owner 上传/下载字节（运维可对账）。
	writeLabeledCounter(&b, "sproxy_usage_upload_bytes_total", "Per-owner upload bytes (usage report)", m.UsageUploadSamples())
	writeLabeledCounter(&b, "sproxy_usage_download_bytes_total", "Per-owner download bytes (usage report)", m.UsageDownloadSamples())

	// 卷健康指标（roadmap §3 P1）：每卷读写延迟/失败率（volume+op 标签）。
	writeLabeledCounter(&b, "sproxy_volume_io_total", "Per-volume IO requests by operation (upload/download)", m.VolumeIOSamples())
	writeLabeledCounter(&b, "sproxy_volume_io_failures_total", "Per-volume IO failures by operation (failure rate = failures / total)", m.VolumeIOFailuresSamples())
	writeLabeledCounter(&b, "sproxy_volume_io_latency_nanos_total", "Per-volume cumulative IO latency in nanoseconds by operation", m.VolumeIOLatencySamples())

	// 卷磁盘水位（roadmap 11.5-⑪）：每卷 usage/capacity gauge（Prometheus 告警模板
	// sproxy_storage_usage_bytes / sproxy_storage_capacity_bytes 消费；从卷集合注入）。
	h.renderVolumeWatermarks(&b, m)

	// 派生状态（卷状态缓存 + 搜索索引）的可观测面（审计 2026-10-29 P2-5）：
	// 命中率判断缓存是否生效/被进程外改动打穿；索引规模与重建次数判断内存与陈旧。
	h.renderIndexCacheMetrics(&b, m)

	// 配额账本（**唯一实现**）的健康面：分片损坏与「降级 ⇒ 拒写」都必须可告警
	// （「限额被静默放宽」是本项目认定的最危险形态）。
	writeMetric(&b, "sproxy_quota_ledger_corrupt_total", "counter",
		"Quota ledger shards found corrupt/unparsable (writes fail closed until repaired)",
		capacity.CorruptLedgerCount())
	writeMetric(&b, "sproxy_quota_ledger_degraded_total", "counter",
		"Writes rejected because the quota ledger is degraded",
		capacity.LedgerDegradedCount())
	// 账本退回内存（卷 FS 承载不了账本）：进程内强制仍在，但**跨进程可见性失效** ⇒
	// 必须可告警（「降级必须可观测」是既定原则，设计 §3.6.2）。
	writeMetric(&b, "sproxy_quota_ledger_mem_fallback_total", "counter",
		"Volumes whose quota ledger fell back to in-memory (cross-process visibility lost; local enforcement retained)",
		capacity.LedgerMemFallbackCount())

	// 基线注入失败（盘满/远端抖动）⇒ 基线丢失 ⇒ 重建后从 0 起算（fail-open）⇒ 该卷已
	// fail-closed 拒写；此计数是唯一的可观测面（独立审核 P2，2026-10-18）。
	writeMetric(&b, "sproxy_quota_ledger_baseline_fail_total", "counter",
		"Volumes whose quota ledger baseline injection failed (volume is fail-closed until fixed)",
		capacity.LedgerBaselineFailCount())

	// 备份/同步任务计数（roadmap 11.5-⑪）：配合 SproxyBackupSyncFailures 告警。
	writeMetric(&b, "sproxy_backup_tasks_total", "counter", "Total backup/sync tasks recorded", m.BackupTasks())
	writeMetric(&b, "sproxy_backup_tasks_failed_total", "counter", "Total backup/sync tasks that failed", m.BackupTasksFailed())

	// rebalance 迁移进度（roadmap 3.3 P1 残余：#440 后补）：按 from/to 维度输出进行中任务的
	// 迁移百分比（gauge；无任务时无样本——与 volume_io 不同，进度是「瞬时」而非「累计」）。
	writeGaugeSamples(&b, "sproxy_rebalance_progress", "Volume rebalance migration progress percent by from/to volume pair (in-flight only)", h.rebalanceProg.Samples())

	// 进程级堆分配指标（roadmap 6.3 P2 内存观测）：runtime.MemStats 拉取时采样。
	writeHeapMetrics(&b)

	// 云端下载指标
	if cm := h.cloudMgr; cm != nil && cm.Metrics() != nil {
		cmMetrics := cm.Metrics()
		writeMetric(&b, "sproxy_cloud_tasks_created", "counter", "Total cloud download tasks created", cmMetrics.TasksCreated.Load())
		writeMetric(&b, "sproxy_cloud_tasks_completed", "counter", "Total cloud download tasks completed", cmMetrics.TasksCompleted.Load())
		writeMetric(&b, "sproxy_cloud_tasks_failed", "counter", "Total cloud download tasks failed", cmMetrics.TasksFailed.Load())
		writeMetric(&b, "sproxy_cloud_tasks_cancelled", "counter", "Total cloud download tasks cancelled", cmMetrics.TasksCancelled.Load())
		writeMetric(&b, "sproxy_cloud_bytes_downloaded", "counter", "Total bytes downloaded by cloud downloader", cmMetrics.BytesDownloaded.Load())
		writeMetric(&b, "sproxy_cloud_active_downloads", "gauge", "Currently active cloud downloads", cmMetrics.ActiveDownloads.Load())
		// 转存（transfer）指标（NH-P2：此前计数但零导出，不可观测）
		writeMetric(&b, "sproxy_cloud_transfers_succeeded", "counter", "Total transfer (download to volume) succeeded", cmMetrics.TransfersSucceeded.Load())
		writeMetric(&b, "sproxy_cloud_transfers_failed", "counter", "Total transfer failed", cmMetrics.TransfersFailed.Load())
		writeMetric(&b, "sproxy_cloud_transfer_target_errors", "counter", "Transfer target volume errors", cmMetrics.TransferTargetErrors.Load())
		writeMetric(&b, "sproxy_cloud_transfer_file_errors", "counter", "Transfer file content errors", cmMetrics.TransferFileErrors.Load())
	}
	metrics.WriteHybridMetrics(&b)
	metrics.WriteGCIDMetrics(&b)

	_, _ = w.Write([]byte(b.String()))
}

// renderIndexCacheMetrics 输出卷状态缓存与搜索索引的指标（gauge/counter）。
// 未装配（nil）⇒ 不输出该组（零回归，不伪造 0）。
func (h *Handlers) renderIndexCacheMetrics(b *strings.Builder, m *Metrics) {
	if h == nil {
		return
	}
	if c := h.stateCache; c != nil {
		hits, misses, entries := c.Stats()
		writeMetric(b, "sproxy_volume_cache_hits_total", "counter",
			"Volume state cache hits (list served without回源)", hits)
		writeMetric(b, "sproxy_volume_cache_misses_total", "counter",
			"Volume state cache misses (list回源 to the volume FS)", misses)
		writeMetric(b, "sproxy_volume_cache_entries", "gauge",
			"Volume state cache entries currently held in memory (LRU)", int64(entries))
	}
	if svc := h.fileService(); svc != nil {
		owners, entries, rebuilds := svc.IndexStats()
		writeMetric(b, "sproxy_search_index_owners", "gauge",
			"Owners whose search index is resident in memory (LRU bounded)", int64(owners))
		writeMetric(b, "sproxy_search_index_entries", "gauge",
			"Search index entries resident in memory", int64(entries))
		writeMetric(b, "sproxy_search_index_rebuilds_total", "counter",
			"Search index full rebuilds (cold start / invalidation / LRU eviction)", int64(rebuilds))
	}
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

		start := time.Now()
		mw := newMetricsResponseWriter(w)
		next.ServeHTTP(mw, r)

		h.metrics.RecordRequest(mw.statusCode)
		h.metrics.RecordRequestLatency(time.Since(start))
	})
}

// MetricsAuth 是 /metrics 的可选令牌门（roadmap 6.x P1；独立指标端口复用）。
// cfg.MetricsToken 为空 → 透传（默认匿名可读零回归）；非空 → 必须携带
// `?token=<t>` 或 `Authorization: Bearer <t>`（常量时间比较），否则 401。
// 独立于 authMiddleware（SproxySig/APIKey）：监控抓取通常不用业务凭据，
// 一个专用只读 token 更符合最小暴露。
func (h *Handlers) MetricsAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfgPtr.Load()
		if cfg == nil || cfg.MetricsToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		want := []byte(cfg.MetricsToken)
		got := []byte(r.URL.Query().Get("token"))
		if len(got) == 0 {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				got = []byte(strings.TrimPrefix(auth, "Bearer "))
			}
		}
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
