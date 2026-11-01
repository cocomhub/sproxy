// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package httpjson 是**JSON 响应写出**的唯一实现：设置 Content-Type、序列化、
// 按状态码写出，并在序列化失败时回 500 + 固定错误体 + 告警日志。
//
// 为什么需要它：`sendJSONResponse`（装配根，328 处调用）、`pkg/files.Service.sendJSON`
// （146 处调用）、`pkg/server/ratelimit` 的 429 回包，此前各持一份等价实现。同一语义
// 多份实现是缺陷温床（`pkg/server/ratelimit` 的副本正是因无法反向 import 装配根而复制，
// 见 #988）——故把实现收到 L0 的 `internal/httpjson`，各层只调用它。
//
// 契约（必须逐字保持，调用方/测试按字节断言）：
//   - 成功：先 `Set(Content-Type, application/json)`，再 `WriteHeader(status)`，最后写
//     `json.Marshal(v)` 的字节（**无尾随换行**）；
//   - 序列化失败：状态码 500，日志 Warn("Encode JSON response failed")，响应体
//     `{"error":"internal server error"}` **带尾随换行**（沿用 json.Encoder 的写出形态）。
package httpjson

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
)

const (
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"

	// errMsgEncodeFailed 是序列化失败时回给客户端的固定文案（不泄漏内部错误）。
	errMsgEncodeFailed = "internal server error"
	// msgEncodeJSONFailed 是序列化失败时的日志文案（与历史实现一致，便于既有日志检索）。
	msgEncodeJSONFailed = "Encode JSON response failed"
)

// logger 是序列化失败时的告警落点。装配根经 SetLogger 在启动与配置热重载时注入
// 当前生效实例（与既有 SetResponseLogger 行为一致）。
var logger atomic.Pointer[slog.Logger]

func init() {
	logger.Store(slog.Default())
}

// SetLogger 设置序列化失败时的告警日志器（nil ⇒ 回退 slog.Default()）。
func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	logger.Store(l)
}

// currentLogger 返回当前生效的告警日志器。
func currentLogger() *slog.Logger {
	if l := logger.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// Send 写 JSON 响应（告警日志用包级实例）。
func Send(w http.ResponseWriter, statusCode int, response any) {
	send(w, statusCode, response, currentLogger())
}

// SendLogged 写 JSON 响应，但告警日志用调用方持有的实例日志器（nil ⇒ 包级实例）。
//
// 领域包（如 pkg/files）持有自己的实例日志器，用它可保持日志路由不变。
func SendLogged(w http.ResponseWriter, statusCode int, response any, log *slog.Logger) {
	if log == nil {
		log = currentLogger()
	}
	send(w, statusCode, response, log)
}

// SendError 写 `{"error": <msg>}` 形态的 JSON 响应（字节与 json.Marshal 一致、无尾随换行）。
func SendError(w http.ResponseWriter, statusCode int, msg string) {
	send(w, statusCode, struct {
		Error string `json:"error"`
	}{Error: msg}, currentLogger())
}

// send 是唯一实现：序列化成功后一次写出；失败则回固定错误体并记 Warn。
func send(w http.ResponseWriter, statusCode int, response any, log *slog.Logger) {
	w.Header().Set(headerContentType, contentTypeJSON)
	buf, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Warn(msgEncodeJSONFailed, "error", err)
		// 沿用 json.Encoder 的写出形态（带尾随换行）以保持既有字节契约。
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errMsgEncodeFailed})
		return
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(buf)
}
