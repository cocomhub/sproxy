// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"net"
	"net/http"
	"time"
)

// WithSlidingWriteDeadline 把 http.Server.WriteTimeout 的语义从「**整响应挂钟**上限」
// 改为「**单次写入的停滞窗口**」：每次 Write/WriteHeader/Flush 都把底层连接的写截止
// 时间推后 grace。于是：
//
//   - 持续有数据流动的长响应（大文件下载、`POST /tunnel` 隧道会话、SSE、S3/WebDAV
//     对象 GET、分享直链）不再被 WriteTimeout 的总时长掐断；
//   - grace 内没有任何写进展（客户端停滞/黑洞/半开连接）仍会被切断 —— 保留停滞保护。
//
// 背景（2026-11-07 线上复盘）：`http.Server.WriteTimeout` 是**整个响应**的挂钟上限。
// 弱网下大文件下载必然超过它（`config.example.yaml` 的 `server_timeouts.write: 30s`
// 即 30s），服务端在响应中途关闭连接 ⇒ 隧道客户端解密流只读到半个帧，报
// `decrypt stream: read chunk: unexpected EOF`。多个并发下载会几乎同时在各自 ~30s
// 处集体失败，客户端只能反复「续传重试」而永远下不完。
//
// 这与客户端侧超时是**两回事**：客户端已由 `client.WithNoClientTimeout` /
// `tunnel.WithNoTimeout` 解除自身 http.Client.Timeout，但服务端的 WriteTimeout 仍会
// 单方面掐断，必须服务端也解除。
//
// grace 传服务端配置的 WriteTimeout 值（`cfg.ServerTimeouts.Write`）；<= 0（未配置，
// 即默认无超时）时原样返回，不做任何包装。
//
// 实现细节：经 http.ResponseController（Go 1.20+）取底层连接的 SetWriteDeadline；
// 包装器显式透传 Unwrap/Hijack/Flush，避免破坏隧道/中继的升级与 SSE 的流式刷新。
func WithSlidingWriteDeadline(next http.Handler, grace time.Duration) http.Handler {
	if next == nil || grace <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&slidingWriteDeadlineWriter{
			ResponseWriter: w,
			rc:             http.NewResponseController(w),
			grace:          grace,
		}, r)
	})
}

// slidingWriteDeadlineWriter 是 WithSlidingWriteDeadline 的 ResponseWriter 包装。
//
// 有意**不实现 io.ReaderFrom**：一次 sendfile 式整文件写无法在中途推后截止时间，
// 会退化成「单个长写」而被 WriteTimeout 掐断；不实现即让 io.Copy 落回 32KiB 分块
// Write（与逐块推后截止时间对齐）。本仓下载链路的 reader 已被
// firstErrReadSeeker/limitResponseWriter 包装，本就不满足 sendfile 前提。
type slidingWriteDeadlineWriter struct {
	http.ResponseWriter
	rc    *http.ResponseController
	grace time.Duration
}

func (w *slidingWriteDeadlineWriter) Write(p []byte) (int, error) {
	w.extend()
	return w.ResponseWriter.Write(p)
}

func (w *slidingWriteDeadlineWriter) WriteHeader(status int) {
	w.extend()
	w.ResponseWriter.WriteHeader(status)
}

// Flush 透传并同等推后截止时间（SSE/分块流依赖）。
func (w *slidingWriteDeadlineWriter) Flush() {
	w.extend()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 暴露底层 ResponseWriter：使 http.ResponseController 与依赖类型断言的调用方
// 能穿透本包装（**必须**，否则内层 SetWriteDeadline/Hijack/Flush 会失效）。
func (w *slidingWriteDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack 透传：隧道/中继/WebSocket 升级依赖它（包装 ResponseWriter 必须保留该能力，
// 否则 w.(http.Hijacker) 断言失败 ⇒ 中继流不可用）。
func (w *slidingWriteDeadlineWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// extend 把写截止时间推后 grace。不支持 SetWriteDeadline 的 ResponseWriter
// （httptest.ResponseRecorder 等）返回 http.ErrNotSupported ⇒ 忽略（退化为原行为，
// 不 panic、不改变语义）。
func (w *slidingWriteDeadlineWriter) extend() {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.grace))
}
