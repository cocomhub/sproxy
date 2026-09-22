// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// pprof_handlers.go 是受认证保护的 /debug/pprof 端点装配（roadmap 6.3 P2 内存观测）。
//
// 设计要点：
//   - 开关：Config.DebugPprofEnabled（默认 false 零回归）。显式开启才暴露，
//     且必须经 authMiddleware（SproxySig/APIKey）认证——未认证 401（安全开关
//     可观测铁律：启用状态启动日志可见，禁静默暴露）。
//   - 路由：/debug/pprof/ 前缀全部挂 srvMux（net/http/pprof 标准库 Index/
//     Cmdline/Profile/Symbol/Trace + 各 Profile 子路径）。Go 1.22 ServeMux 的
//     前缀模式 "GET /debug/pprof/" 会匹配所有子路径（/debug/pprof/heap 等），
//     由 pprof.Index 内部按 path 分发 profile——故只需注册前缀一个入口即可覆盖
//     heap/goroutine/allocs/block/mutex + cmdline/symbol/trace。
//   - heap 指标：/metrics 暴露 sproxy_heap_alloc_bytes / sproxy_heap_objects /
//     sproxy_gc_cycles（runtime.ReadMemStats 拉取时采样，进程级无标签）。

package server

import (
	"net/http"
	"net/http/pprof"
	"runtime"
	"strings"
)

// pprofMux 是 /debug/pprof 子 mux：注册标准库 pprof 处理器（Index 覆盖全部
// profile 子路径 + cmdline/symbol/trace）。独立 mux 便于整体经 authMiddleware 包装。
func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", pprof.Index)
	mux.HandleFunc("/cmdline", pprof.Cmdline)
	mux.HandleFunc("/profile", pprof.Profile)
	mux.HandleFunc("/symbol", pprof.Symbol)
	mux.HandleFunc("/trace", pprof.Trace)
	// Index 内已按 path 分发 heap/goroutine/allocs/block/mutex 等 Profile，
	// 无需逐个显式注册（net/http/pprof.Index 的 defaultServeMux 语义即如此）。
	return mux
}

// registerPprofRoutes 装配受认证保护的 /debug/pprof 端点（仅 DebugPprofEnabled
// 开启时挂载；关闭 = 404 零回归）。启用状态日志可观测（安全开关铁律）。
func (h *Handlers) registerPprofRoutes(srvMux *http.ServeMux) {
	cfg := h.cfgPtr.Load()
	if cfg == nil || !cfg.DebugPprofEnabled {
		return
	}
	h.logger.Info("pprof 端点已启用（受认证保护）", "path", "/debug/pprof/")
	srvMux.Handle("GET /debug/pprof/", h.authMiddleware(pprofMux().ServeHTTP))
}

// heapMetricsSamples 采样 runtime.MemStats 并追加到 metrics 输出（进程级分配指标）。
func writeHeapMetrics(b *strings.Builder) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	writeMetric(b, "sproxy_heap_alloc_bytes", "gauge", "Current heap bytes allocated (runtime.MemStats.HeapAlloc)", int64(ms.HeapAlloc))
	writeMetric(b, "sproxy_heap_objects", "gauge", "Current heap object count (runtime.MemStats.HeapObjects)", int64(ms.HeapObjects))
	writeMetric(b, "sproxy_gc_cycles", "counter", "Total number of completed GC cycles (runtime.MemStats.NumGC)", int64(ms.NumGC))
}
