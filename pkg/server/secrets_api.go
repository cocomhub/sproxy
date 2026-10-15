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
	"sort"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
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

// secretsDefaultKey 是「显式默认 secrets 卷」标记键（`volumes[].extra.default: true`）：
// 多 secrets 卷并存时，被标记者为 `/api/secrets` 管理面的操作对象。缺省不设（false）。
const secretsDefaultKey = "default"

// secretsVolumeTier 是默认 secrets 卷的**优先级档**（数值小者优先）：
//
//	0 = 显式默认标记（extra.default: true）；
//	1 = 基础 secrets 卷（非嵌套派生，无 extra.source）；
//	2 = 嵌套派生的 secrets 卷（区域视图上的 secrets，由 wrapper 装配在其它卷的子目录上）。
//
// 「基础卷优先于嵌套派生卷」的理由：`/api/secrets` 是**密钥管理面**，其语义读者是「服务级
// 密钥目录」；嵌套派生的 secrets 卷是某个子区域的视图，语义上是「该区域的密钥」，不应因
// 配置声明顺序（或规范化后 All() 的顺序）而抢占管理面。
func secretsVolumeTier(v volume.Volume) int {
	if b, _ := v.Extra[secretsDefaultKey].(bool); b {
		return 0
	}
	if raw, _ := v.Extra[enclosure.SourceKey].(string); strings.TrimSpace(raw) != "" {
		return 2
	}
	return 1
}

// secretsManager 返回**默认 secrets 卷**的 Manager，未装配/不可用返回 nil（调用方 fail-closed：
// 不可用即报错，不静默写错卷）。
//
// 选取规则（**确定性**，彻底消除「配置声明顺序首个胜出」歧义——历史实现对 `volSet.All()` 取
// 首个 `type: secrets`，既是顺序依赖，也会被归一后出现的嵌套 secrets 卷抢先命中）：
//  1. 候选 = 已装配外部句柄且能反取 `*secrets.Manager` 的 `type: secrets` 卷；
//  2. 按 `secretsVolumeTier` 升序（显式默认标记 > 基础 secrets 卷 > 嵌套派生）；
//  3. 同档按**卷名升序**（声明序之外唯一的稳定序）。
//
// 不依赖 `volSet.DefaultExternal()`：那是「首个登记的外部卷」，用户名下先装配 baidupcs 等
// 时它就不是 secrets 卷（历史注释已指出该陷阱）。
func (h *Handlers) secretsManager() *secrets.Manager {
	if h.volSet == nil {
		return nil
	}
	cands := make([]volume.Volume, 0, 2)
	for _, v := range h.volSet.All() {
		if v.Type != secretVolumeType {
			continue
		}
		if secrets.ManagerOfExternal(h.volSet.External(v.Name)) == nil {
			continue // 未装配/装配失败：不可用候选不进排序
		}
		cands = append(cands, v)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		ti, tj := secretsVolumeTier(cands[i]), secretsVolumeTier(cands[j])
		if ti != tj {
			return ti < tj
		}
		return cands[i].Name < cands[j].Name
	})
	return secrets.ManagerOfExternal(h.volSet.External(cands[0].Name))
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
// msgSecretsUnavailable 是 secrets 卷未装配/不可用的共用错误文案（S1192 收敛）。
const msgSecretsUnavailable = "secrets 卷未装配/不可用"

func (h *Handlers) createSecretHandler(w http.ResponseWriter, r *http.Request) {
	mgr := h.secretsManager()
	if mgr == nil {
		http.Error(w, msgSecretsUnavailable, http.StatusServiceUnavailable)
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
		http.Error(w, msgSecretsUnavailable, http.StatusServiceUnavailable)
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
		http.Error(w, msgSecretsUnavailable, http.StatusServiceUnavailable)
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
		http.Error(w, msgSecretsUnavailable, http.StatusServiceUnavailable)
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
