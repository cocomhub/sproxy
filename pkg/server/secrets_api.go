// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// secrets_api.go 是 secret 卷的 HTTP 管理面（接线：CLI/Web 创建/列表/导出/删除 secret）。
//
// 与通用文件 API 的关系（客观约束，2026-10-04 核实）：secrets 卷是 **ExternalBackend**
// （secret_register.go 以 AddExternalVolume 装配到 external map），而通用 /upload /download
// /api/files 的 ?volume= 只解析**本地卷**（volSet.Root/volumeTenant 走 roots/caches），
// 外部卷不被通用文件 API 命中。故 secret 管理必须走**专用 /api/secrets 端点**——
// 这同时是正确安全设计（secret 不混入普通文件列表、不被通用删除/列表误管理）。
//
// 服务端只做「校验 + 落盘到 secrets 卷」，**不参与任何口令/密钥派生**：
//   - mode=random：服务端 crypto/rand 生成 32B→hex，返回给创建者（供立即备份）。
//   - mode=import / passphrase：客户端传 `value`（本地派生结果），服务端校验 64-hex 后写卷。
//
// 原始口令永不传输到本层。

// secretVolumeType 是 secret 封装卷的 volume.Type（装配层 AddExternalVolume 用）。
const secretVolumeType = "secrets"

// secretsManager 返回装配后的 secrets 卷 Manager（按卷类型定位，不依赖默认卷次序——
// DefaultExternal() 是首个外部卷，可能不是 secrets（用户先装配 baidupcs 等）。
// 未装配/装配失败返回 nil（调用方 fail-closed：不可用即报错，不静默写错卷）。
func (h *Handlers) secretsManager() *secrets.Manager {
	if h.volSet == nil {
		return nil
	}
	for _, v := range h.volSet.All() {
		if v.Type != secretVolumeType {
			continue
		}
		be := h.volSet.External(v.Name)
		if be == nil {
			continue
		}
		if mgr := secrets.ManagerOfExternal(be); mgr != nil {
			return mgr
		}
	}
	return nil
}

// createSecretRequest 是 POST /api/secrets 请求体。
//   - mode: "random"（缺省）→ 服务端生成随机 secret（origin=random），响应返回 value 供备份；
//   - mode: "import"/"passphrase" → 客户端传 `value`（32B hex 本地派生结果）到 secrets 卷。
type createSecretRequest struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Value  string `json:"value"`
	Origin string `json:"origin"` // 来源标注（random/passphrase/import）；信息性，不影响落盘内容
}

// secretCreateResponse 是 POST /api/secrets 响应。
type secretCreateResponse struct {
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"` // 仅 random 模式返回（供立即备份）
	Origin  string `json:"origin"`
	Message string `json:"message,omitempty"`
}

// createSecretHandler 处理 POST /api/secrets：创建（追加/覆盖）一个 secret。
// 客户端负责派生/生成，本 handler 只校验+落盘。
func (h *Handlers) createSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretsManager()
	if mgr == nil {
		http.Error(w, "secrets 卷未装配/不可用", http.StatusServiceUnavailable)
		return
	}
	var req createSecretRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求体解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	switch mode {
	case "random", "":
		key, err := mgr.Create(r.Context(), req.Name)
		if err != nil {
			// 非法名（空/含分隔符/`.`/`..`）→ 400；其余 IO 错误 → 500。
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "非法 secret 名") {
				status = http.StatusBadRequest
			}
			http.Error(w, "secret 创建失败: "+err.Error(), status)
			return
		}
		sendJSONResponse(w, secretCreateResponse{
			Name: req.Name, Value: string(key), Origin: "random",
		}, http.StatusOK)
		return
	case "import", "passphrase":
		if strings.TrimSpace(req.Value) == "" {
			http.Error(w, "import/passphrase 模式需提供 value（64 位小写 hex）", http.StatusBadRequest)
			return
		}
		_, err := mgr.Import(r.Context(), req.Name, []byte(req.Value))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		origin := req.Origin
		if origin == "" {
			origin = "import"
		}
		sendJSONResponse(w, secretCreateResponse{
			Name: req.Name, Origin: origin, Message: "ok",
		}, http.StatusOK)
		return
	default:
		http.Error(w, fmt.Sprintf("未知 mode %q（random/import/passphrase）", mode), http.StatusBadRequest)
	}
}

// secretsListResponse 是 GET /api/secrets 响应。
type secretsListResponse struct {
	Secrets []string `json:"secrets"`
}

// listSecretsHandler 处理 GET /api/secrets：列出卷内全部 secret 名（排序）。
func (h *Handlers) listSecretsHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretsManager()
	if mgr == nil {
		http.Error(w, "secrets 卷未装配/不可用", http.StatusServiceUnavailable)
		return
	}
	names, err := mgr.List(r.Context())
	if err != nil {
		http.Error(w, "secrets 列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, secretsListResponse{Secrets: names}, http.StatusOK)
}

// exportSecretHandler 处理 GET /api/secrets/{name}：导出 secret 内容（明文 hex）。
// 供备份/迁移（CLI secret export / web 下载）。不存在 fail-closed。
func (h *Handlers) exportSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretsManager()
	if mgr == nil {
		http.Error(w, "secrets 卷未装配/不可用", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	data, err := mgr.Read(r.Context(), name)
	if err != nil {
		http.Error(w, "读取 secret 失败: "+err.Error(), http.StatusNotFound)
		return
	}
	// 明文返回（hex 文本）；调用方负责后续保护（默认加密/自保管）。
	sendJSONResponse(w, secretCreateResponse{
		Name: name, Value: string(data), Origin: "export",
	}, http.StatusOK)
}

// deleteSecretHandler 处理 DELETE /api/secrets/{name}：删除 secret。
// 不存在 fail-closed（不静默 no-op）。
func (h *Handlers) deleteSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretsManager()
	if mgr == nil {
		http.Error(w, "secrets 卷未装配/不可用", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	// 先确认存在（fail-closed：删除不存在返回 404，不静默成功）。
	exists, err := mgr.Exists(r.Context(), name)
	if err != nil || !exists {
		http.Error(w, "secret 不存在", http.StatusNotFound)
		return
	}
	if err := h.deleteSecretFile(mgr, name); err != nil {
		http.Error(w, "删除 secret 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]string{"name": name, "deleted": "true"}, http.StatusOK)
}

// deleteSecretFile 删除 secret 文件（fs.Delete，Exists 已确认存在）。
func (h *Handlers) deleteSecretFile(mgr *secrets.Manager, name string) error {
	// secrets.Manager 唯一句柄经 fs.Delete 删除（name 已由 Exists 确认合法）。
	return mgr.Remove(context.Background(), name)
}
