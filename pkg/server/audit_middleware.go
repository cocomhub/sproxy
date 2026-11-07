// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// audit_middleware.go 是**传输层审计兜底**（设计 §2.5/§8.2 R35-b）。
//
// 目的：**覆盖完备性不依赖人** —— 任何变更型请求（POST/PUT/PATCH/DELETE）至少留下一条可追溯
// 记录（谁/何时/哪个路由/结果/耗时），即使该路由的业务语义审计尚未接入。
//
// 语义边界（必须明确，否则门禁假绿）：
//   - 本中间件产生的事件带 `fidelity=transport` 且 action 形如 `http.post`；
//   - **不计入业务覆盖达标**（业务语义审计必须由使用方显式 Emit：action/object/volume/result）；
//   - 门禁 R35-b 以「业务语义审计」为判据，transport 事件不能替代它。
const (
	// auditFidelityTransport 标记传输层兜底事件（与业务语义审计区分）。
	auditFidelityTransport = "transport"
	// auditTransportActionPrefix 是兜底事件的 action 前缀。
	auditTransportActionPrefix = "http."
)

// auditMiddleware 对变更型请求记录传输层审计事件（GET/HEAD/OPTIONS 跳过：只读不产生变更）。
func (h *Handlers) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.auditSvc == nil || !isMutatingMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &auditStatusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		// 审计写入绝不阻断业务：Emit 为 best-effort（fail-closed 时也仅返回错误，不影响响应）。
		_ = h.auditSvc.Emit(r.Context(), audit.Entry{
			Scope:   audit.ScopeID(systemAuditScope),
			Kind:    audit.KindEvent,
			Type:    audit.TypeAPI,
			Action:  auditTransportActionPrefix + strings.ToLower(r.Method),
			Actor:   ActorFrom(r.Context()),
			Owner:   ActorFrom(r.Context()), // 租户键 = AK（设计 §5.7 规则 1）
			Mesh:    MeshFrom(r.Context()),
			ObjType: "route",
			Object:  r.URL.Path,
			Result:  auditResultForStatus(rec.status),
			Detail:  fmt.Sprintf("status=%d dur_ms=%d fidelity=%s", rec.status, time.Since(start).Milliseconds(), auditFidelityTransport),
			Meta: map[string]any{
				"fidelity": auditFidelityTransport,
				"status":   rec.status,
				"method":   r.Method,
				"path":     r.URL.Path,
			},
			TS: time.Now(),
		})
	})
}

// isMutatingMethod 报告 HTTP 方法是否会产生变更（只读方法不记传输层审计）。
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// auditResultForStatus 把 HTTP 状态映射为审计结果（与既有 AuditResult* 取值一致）。
func auditResultForStatus(status int) audit.Result {
	switch {
	case status < 400:
		return audit.ResultSuccess
	case status < 500:
		return audit.ResultDenied // 客户端侧拒绝（校验失败/权限不足）
	default:
		return audit.ResultError
	}
}

// auditStatusRecorder 记录响应状态码（不改变任何既有行为：透传 ResponseWriter）。
type auditStatusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader 记录状态码后透传。
func (r *auditStatusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap 暴露底层 ResponseWriter：使 http.ResponseController（Flush/Hijack/SetDeadline…）
// 与依赖类型断言的调用方能穿透本包装（**必须**，否则隧道/中继/流式响应会失效）。
func (r *auditStatusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Hijack 透传：隧道/中继/WebSocket 升级依赖它（包装 ResponseWriter 必须保留该能力）。
func (r *auditStatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Flush 透传：流式响应（SSE/分块）需要。
func (r *auditStatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Write 隐式 200（未显式 WriteHeader 时）。
func (r *auditStatusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// auditVolumeMiddleware 把「本次请求指向的卷」绑定进 ctx（审计第 6 维，评审 R13）。
//
// 为什么放在中间件而不是逐 handler：卷名在各端点以 `?volume=` / 路径变量等多种方式出现，
// 逐点绑定必然漏（130 个 RecordAudit 调用点里约 1/3 与卷相关）。请求携带卷名时统一绑定；
// 默认卷/后端解析出的卷由解析点用 withAuditVolume 补充（缺省不写 ⇒ 不伪造）。
func (h *Handlers) auditVolumeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("volume")
		if name == "" {
			name = r.PathValue("volume")
		}
		if name == "" {
			next.ServeHTTP(w, r)
			return
		}
		// R14 C12：**只绑定真实存在的卷**。此前把请求里的原始值直接绑进 ctx ⇒ 单卷部署下
		// `?volume=finance` 会伪造审计归属，并压制「单卷兜底」（真值本可唯一推出）。
		// 不存在/未授权一律忽略（handler 自身会返回 404/403，审计不得采信未验证输入）。
		if h.volSet != nil {
			if _, ok := h.volSet.ByName(name); !ok {
				next.ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withAuditVolume(r.Context(), name)))
	})
}
