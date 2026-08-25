// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

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
}

// NewMetrics 创建并初始化 Metrics。
func NewMetrics() *Metrics {
	return &Metrics{}
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
			total.FramesSent.Add(mm.FramesSent.Load())
			total.FramesReceived.Add(mm.FramesReceived.Load())
			total.PingsSent.Add(mm.PingsSent.Load())
			total.PongsReceived.Add(mm.PongsReceived.Load())
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
		writeMetric(&b, "sproxy_mux_bytes_read", "counter", "Mux bytes read", mm.Streams.BytesRead.Load())
		writeMetric(&b, "sproxy_mux_bytes_written", "counter", "Mux bytes written", mm.Streams.BytesWritten.Load())
		writeMetric(&b, "sproxy_mux_frames_sent", "counter", "Mux frames sent", mm.FramesSent.Load())
		writeMetric(&b, "sproxy_mux_frames_received", "counter", "Mux frames received", mm.FramesReceived.Load())
		writeMetric(&b, "sproxy_mux_pings_sent", "counter", "Mux pings sent", mm.PingsSent.Load())
		writeMetric(&b, "sproxy_mux_pongs_received", "counter", "Mux pongs received", mm.PongsReceived.Load())
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
	// 云端下载指标
	if cm := h.cloudMgr; cm != nil && cm.metrics != nil {
		cmMetrics := cm.metrics
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

// metricsResponseWriter 包装 http.ResponseWriter，捕获状态码。
type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func newMetricsResponseWriter(w http.ResponseWriter) *metricsResponseWriter {
	return &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

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
