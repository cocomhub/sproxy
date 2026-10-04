// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
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
//
// 错误分类（Important-2 修复，哨兵错误替代脆弱文案匹配）：
//   - 客户端输入错误（非法名/非法值/未知 mode）→ 400；
//   - 服务端 IO/落盘故障 → 500（不误归为客户端错误）。
//
// **响应脱敏 + 服务端日志记原始错误**（2026-10-04 用户要求可排障）：客户端只见
// 脱敏文案（不泄漏 FS 路径/内部细节）；原始错误（含 %w 链）经 h.logger.Error 落服务端
// 日志——安全与可排查性兼得，不违反「禁止静默失败」纪律。
func (h *Handlers) createSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretsManager()
	if mgr == nil {
		http.Error(w, "secrets 卷未装配/不可用", http.StatusServiceUnavailable)
		return
	}
	var req createSecretRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求体解析失败", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "random", "":
		h.createSecretRandom(w, r, mgr, req.Name)
	case "import", "passphrase":
		h.createSecretImport(w, r, mgr, req)
	default:
		http.Error(w, fmt.Sprintf("未知 mode %q（random/import/passphrase）", strings.ToLower(strings.TrimSpace(req.Mode))), http.StatusBadRequest)
	}
}

// createSecretRandom 处理 mode=random：服务端生成随机 secret，返回 value 供备份。
func (h *Handlers) createSecretRandom(w http.ResponseWriter, r *http.Request, mgr *secrets.Manager, name string) {
	key, err := mgr.Create(r.Context(), name)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidSecretName) {
			http.Error(w, "secret 创建失败", http.StatusBadRequest)
			return
		}
		h.logger.Error("创建随机 secret 落盘失败", "name", name, "err", err)
		http.Error(w, "secret 创建失败", http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, secretCreateResponse{
		Name: name, Value: string(key), Origin: "random",
	}, http.StatusOK)
}

// createSecretImport 处理 mode=import/passphrase：客户端上传本地派生结果，校验后落盘。
func (h *Handlers) createSecretImport(w http.ResponseWriter, r *http.Request, mgr *secrets.Manager, req createSecretRequest) {
	if strings.TrimSpace(req.Value) == "" {
		http.Error(w, "import/passphrase 模式需提供 value（64 位小写 hex）", http.StatusBadRequest)
		return
	}
	_, err := mgr.Import(r.Context(), req.Name, []byte(req.Value))
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidSecretName) || errors.Is(err, secrets.ErrInvalidSecretValue) {
			http.Error(w, "secret 导入失败", http.StatusBadRequest)
			return
		}
		h.logger.Error("导入 secret 落盘失败", "name", req.Name, "err", err)
		http.Error(w, "secret 导入失败", http.StatusInternalServerError)
		return
	}
	origin := req.Origin
	if origin == "" {
		origin = "import"
	}
	sendJSONResponse(w, secretCreateResponse{
		Name: req.Name, Origin: origin, Message: "ok",
	}, http.StatusOK)
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
		h.logger.Error("secrets 列表失败", "err", err)
		http.Error(w, "secrets 列表失败", http.StatusInternalServerError)
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
		// 非法名（路径穿越等）→ 400；不存在 → 404；IO 故障 → 500。
		status := http.StatusNotFound
		if errors.Is(err, secrets.ErrInvalidSecretName) {
			status = http.StatusBadRequest
		} else if !errors.Is(err, secrets.ErrNotFound) {
			status = http.StatusInternalServerError
			h.logger.Error("读取 secret 失败", "name", name, "err", err)
		}
		http.Error(w, "读取 secret 失败", status)
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
		status := http.StatusNotFound
		if err != nil && errors.Is(err, secrets.ErrInvalidSecretName) {
			status = http.StatusBadRequest
		}
		if err != nil && !errors.Is(err, secrets.ErrInvalidSecretName) {
			h.logger.Error("删除前探测 secret 失败", "name", name, "err", err)
		}
		http.Error(w, "secret 不存在", status)
		return
	}
	if err := h.deleteSecretFile(r.Context(), mgr, name); err != nil {
		h.logger.Error("删除 secret 失败", "name", name, "err", err)
		http.Error(w, "删除 secret 失败", http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]string{"name": name, "deleted": "true"}, http.StatusOK)
}

// deleteSecretFile 删除 secret 文件（fs.Delete，Exists 已确认存在）。
func (h *Handlers) deleteSecretFile(ctx context.Context, mgr *secrets.Manager, name string) error {
	// secrets.Manager 唯一句柄经 fs.Delete 删除（name 已由 Exists 确认合法）。
	return mgr.Remove(ctx, name)
}
