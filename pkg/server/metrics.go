// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics 使用 atomic 计数器收集请求统计数据。
// 所有字段对齐到 64-bit 边界，确保 32-bit 平台安全。
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

	fmt.Fprintf(w, "# HELP sproxy_requests_total Total HTTP requests\n")
	fmt.Fprintf(w, "# TYPE sproxy_requests_total counter\n")
	fmt.Fprintf(w, "sproxy_requests_total %d\n\n", m.RequestsTotal.Load())

	fmt.Fprintf(w, "# HELP sproxy_requests_2xx HTTP 2xx requests\n")
	fmt.Fprintf(w, "# TYPE sproxy_requests_2xx counter\n")
	fmt.Fprintf(w, "sproxy_requests_2xx %d\n\n", m.Requests2XX.Load())

	fmt.Fprintf(w, "# HELP sproxy_requests_4xx HTTP 4xx requests\n")
	fmt.Fprintf(w, "# TYPE sproxy_requests_4xx counter\n")
	fmt.Fprintf(w, "sproxy_requests_4xx %d\n\n", m.Requests4XX.Load())

	fmt.Fprintf(w, "# HELP sproxy_requests_5xx HTTP 5xx requests\n")
	fmt.Fprintf(w, "# TYPE sproxy_requests_5xx counter\n")
	fmt.Fprintf(w, "sproxy_requests_5xx %d\n\n", m.Requests5XX.Load())

	fmt.Fprintf(w, "# HELP sproxy_bytes_uploaded Total bytes uploaded\n")
	fmt.Fprintf(w, "# TYPE sproxy_bytes_uploaded counter\n")
	fmt.Fprintf(w, "sproxy_bytes_uploaded %d\n\n", m.BytesUploaded.Load())

	fmt.Fprintf(w, "# HELP sproxy_bytes_downloaded Total bytes downloaded\n")
	fmt.Fprintf(w, "# TYPE sproxy_bytes_downloaded counter\n")
	fmt.Fprintf(w, "sproxy_bytes_downloaded %d\n\n", m.BytesDownloaded.Load())

	fmt.Fprintf(w, "# HELP sproxy_active_connections Currently active connections\n")
	fmt.Fprintf(w, "# TYPE sproxy_active_connections gauge\n")
	fmt.Fprintf(w, "sproxy_active_connections %d\n\n", m.ActiveConnections.Load())

	fmt.Fprintf(w, "# HELP sproxy_files_uploaded Total files uploaded\n")
	fmt.Fprintf(w, "# TYPE sproxy_files_uploaded counter\n")
	fmt.Fprintf(w, "sproxy_files_uploaded %d\n\n", m.FilesUploaded.Load())

	fmt.Fprintf(w, "# HELP sproxy_files_downloaded Total files downloaded\n")
	fmt.Fprintf(w, "# TYPE sproxy_files_downloaded counter\n")
	fmt.Fprintf(w, "sproxy_files_downloaded %d\n\n", m.FilesDownloaded.Load())

	fmt.Fprintf(w, "# HELP sproxy_files_deleted Total files deleted\n")
	fmt.Fprintf(w, "# TYPE sproxy_files_deleted counter\n")
	fmt.Fprintf(w, "sproxy_files_deleted %d\n\n", m.FilesDeleted.Load())
	if mm := h.muxMetrics; mm != nil {
		fmt.Fprintf(w, "# HELP sproxy_mux_streams_opened Mux streams opened\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_streams_opened counter\n")
		fmt.Fprintf(w, "sproxy_mux_streams_opened %d\n\n", mm.Streams.Opened.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_bytes_read Mux bytes read\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_bytes_read counter\n")
		fmt.Fprintf(w, "sproxy_mux_bytes_read %d\n\n", mm.Streams.BytesRead.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_bytes_written Mux bytes written\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_bytes_written counter\n")
		fmt.Fprintf(w, "sproxy_mux_bytes_written %d\n\n", mm.Streams.BytesWritten.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_frames_sent Mux frames sent\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_frames_sent counter\n")
		fmt.Fprintf(w, "sproxy_mux_frames_sent %d\n\n", mm.FramesSent.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_frames_received Mux frames received\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_frames_received counter\n")
		fmt.Fprintf(w, "sproxy_mux_frames_received %d\n\n", mm.FramesReceived.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_pings_sent Mux pings sent\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_pings_sent counter\n")
		fmt.Fprintf(w, "sproxy_mux_pings_sent %d\n\n", mm.PingsSent.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_pongs_received Mux pongs received\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_pongs_received counter\n")
		fmt.Fprintf(w, "sproxy_mux_pongs_received %d\n\n", mm.PongsReceived.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_errors Mux errors\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_errors counter\n")
		fmt.Fprintf(w, "sproxy_mux_errors %d\n\n", mm.Errors.Load())
		fmt.Fprintf(w, "# HELP sproxy_mux_stream_errors Mux stream errors\n")
		fmt.Fprintf(w, "# TYPE sproxy_mux_stream_errors counter\n")
		fmt.Fprintf(w, "sproxy_mux_stream_errors %d\n\n", mm.Streams.Errors.Load())
	}
	// Hub 级指标
	if rt := h.routeTable; rt != nil {
		fmt.Fprintf(w, "# HELP sproxy_hub_nodes_connected Current number of connected relay nodes\n")
		fmt.Fprintf(w, "# TYPE sproxy_hub_nodes_connected gauge\n")
		fmt.Fprintf(w, "sproxy_hub_nodes_connected %d\n\n", rt.NodeCount())
	}
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

// metricsMiddleware 自动记录请求状态码和活跃连接数。
// 在 Handler 链外层使用，捕获所有响应的状态码。
func (h *Handlers) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.metrics.ActiveConnections.Add(1)
		defer h.metrics.ActiveConnections.Add(-1)

		mw := newMetricsResponseWriter(w)
		next.ServeHTTP(mw, r)

		h.metrics.RecordRequest(mw.statusCode)
	})
}
