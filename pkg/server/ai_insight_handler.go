// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_insight_handler.go 是 /api/ai/summarize + /api/ai/tag 端点实现：
//
//   - POST /api/ai/summarize?filename=<rel> → 200 {summary} | 400 未启用/非文本 | 404 | 502 网关失败
//   - POST /api/ai/tag?filename=<rel> → 200 {tags:[...]} | 同上
//   - owner 隔离（只能洞察自己文件）+ ValidateFilePath 防穿越。

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/llmgate"
	"github.com/cocomhub/sproxy/pkg/pathguard"
)

// sampleLimit 是抽样字节上限（首 4KiB，成本有界）。
const sampleLimit = 4 << 10

// aiSample 从租户存储读取文件首 4KiB（OpenDecrypted 经 at-rest 解密）。
// 返回抽样文本 + mtime；空文本 → ErrNotTextFile。
func (h *Handlers) aiSample(owner, rel string) (string, int64, error) {
	tenant := h.tenantFor(owner)
	root := tenant.Root()
	fi, err := root.Stat(rel)
	if err != nil {
		return "", 0, err
	}
	rc, err := root.OpenDecrypted(rel)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	mtime := fi.ModTime().UnixNano()
	data, err := io.ReadAll(io.LimitReader(rc, sampleLimit))
	if err != nil {
		return "", 0, err
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", 0, ErrNotTextFile
	}
	return text, mtime, nil
}

// handleSummarize 处理 /api/ai/summarize（owner 隔离：handler 由认证层注入 owner）。
func (h *Handlers) handleAISummarize(w http.ResponseWriter, r *http.Request, owner string, ai *AIInsight) {
	if ai == nil || ai.gate == nil {
		writeAIError(w, http.StatusBadRequest, "AI 洞察未启用")
		return
	}
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		writeAIError(w, http.StatusBadRequest, "filename 必填")
		return
	}
	if _, err := pathguard.ValidateFilePath(filename); err != nil {
		writeAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	text, mtime, err := h.aiSample(owner, filename)
	if err != nil {
		if err == ErrNotTextFile {
			writeAIError(w, http.StatusBadRequest, "非文本文件")
			return
		}
		writeAIError(w, http.StatusNotFound, "文件不存在")
		return
	}
	// 缓存命中（mtime 未变）
	if got, _, ok, _ := ai.cache.Get(r.Context(), owner, filename, "sum", mtime); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(got)
		return
	}
	// 配额前置门（roadmap 11.9-⑦）：超限 429 不调网关 + quota_exceeded 审计。
	if ai.quota != nil {
		est := len(text) / 4
		if qerr := ai.quota.CheckAndCharge(owner, est); qerr != nil {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: ActionAISummarize, ObjectType: "file", Object: filename,
				Result: "quota_exceeded", Detail: qerr.Error(),
			})
			writeAIError(w, http.StatusTooManyRequests, "AI 配额超限: "+qerr.Error())
			return
		}
	}
	system, user := insightPrompts(filename, text)
	resp, err := ai.gate.Advise(r.Context(), system, user)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: ActionAISummarize, ObjectType: "file", Object: filename,
			Result: AuditResultError, Detail: err.Error(),
		})
		writeAIError(w, http.StatusBadGateway, "AI 网关失败: "+err.Error())
		return
	}
	summary := truncateRunes(resp, maxSummaryRunes)
	h.RecordAudit(r.Context(), AuditEvent{
		Action: ActionAISummarize, ObjectType: "file", Object: filename,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("tokens≈%d", len(text)/4),
	})
	body, _ := json.Marshal(map[string]string{"summary": summary})
	_ = ai.cache.Put(r.Context(), owner, filename, "sum", mtime, body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleAITag 处理 /api/ai/tag。
func (h *Handlers) handleAITag(w http.ResponseWriter, r *http.Request, owner string, ai *AIInsight) {
	if ai == nil || ai.gate == nil {
		writeAIError(w, http.StatusBadRequest, "AI 洞察未启用")
		return
	}
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		writeAIError(w, http.StatusBadRequest, "filename 必填")
		return
	}
	if _, err := pathguard.ValidateFilePath(filename); err != nil {
		writeAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	text, mtime, err := h.aiSample(owner, filename)
	if err != nil {
		if err == ErrNotTextFile {
			writeAIError(w, http.StatusBadRequest, "非文本文件")
			return
		}
		writeAIError(w, http.StatusNotFound, "文件不存在")
		return
	}
	if got, _, ok, _ := ai.cache.Get(r.Context(), owner, filename, "tag", mtime); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(got)
		return
	}
	if ai.quota != nil {
		est := len(text) / 4
		if qerr := ai.quota.CheckAndCharge(owner, est); qerr != nil {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: ActionAITag, ObjectType: "file", Object: filename,
				Result: "quota_exceeded", Detail: qerr.Error(),
			})
			writeAIError(w, http.StatusTooManyRequests, "AI 配额超限: "+qerr.Error())
			return
		}
	}
	system, user := insightPrompts(filename, text)
	resp, err := ai.gate.Advise(r.Context(), system, user)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: ActionAITag, ObjectType: "file", Object: filename,
			Result: AuditResultError, Detail: err.Error(),
		})
		writeAIError(w, http.StatusBadGateway, "AI 网关失败: "+err.Error())
		return
	}
	tags := normalizeTags(resp)
	h.RecordAudit(r.Context(), AuditEvent{
		Action: ActionAITag, ObjectType: "file", Object: filename,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("tags=%d", len(tags)),
	})
	body, _ := json.Marshal(map[string]any{"tags": tags})
	_ = ai.cache.Put(r.Context(), owner, filename, "tag", mtime, body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// writeAIError 写 JSON 错误。
func writeAIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

var _ = llmgate.Client{}
var _ = slog.Default
