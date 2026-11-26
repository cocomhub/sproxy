// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_settings_api.go 实现**卷详情与设置分区**的 HTTP 面
// （设计 docs/designs/2026-11-26-secretdata-block-policy-rules.md §8）。
//
// 形态选择（用户裁定 2026-11-26「每个卷新增详情按钮，在 secretdata 卷详细中新增分块规则模块，
// 后续不同类型的卷新增配置功能都采用类似方式」）：详情与设置是**通用容器**——分区列表由后端
// 能力（registry.SettingsProvider/SettingsWriter）驱动，装配层不含任何卷型专有分支。后续卷型
// 新增配置只需实现能力并声明字段，**不改路由、不改前端框架**。
//
// 路径为什么是 `/{name}/detail` 而不是 `/{name}`：卷名只受 storage.ValidSegmentName 约束，
// `export`/`user`/`refresh` 等都是**合法卷名**；若详情挂在两段通配路径上，名为 export 的卷
// 永远打不开详情（字面段优先于通配段）。三段形态与既有 `/meta-audit`、`/orphans`、`/trace`、
// `/explain` 同风格，结构性无歧义（已用最小 ServeMux 用例实测）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/internal/httpjson"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// auditActionVolumeSettingsUpdate 是「写卷设置」的业务审计动作（R35-b 登记项）。
const auditActionVolumeSettingsUpdate = "volume_settings_update"

// volumeSettingsMaxBody 是设置写入请求体上限（规则集是文本，1 MiB 远超实际需要）。
const volumeSettingsMaxBody = 1 << 20

// volumeSettingsAccess 是卷设置面的访问判定结果。
type volumeSettingsAccess struct {
	vol volume.Volume
	be  registry.ExternalBackend
}

// resolveVolumeSettingsAccess 执行权限门（顺序不可交换，见设计 §8.1）：
//
//	① 可见性：卷必须在 egressVisibleView(AllowedVolumes(all, owner), owner) 内，否则 404
//	   （他人私有卷是 owner-only ACL ⇒ 在这里就 404，不泄露「该卷存在且属于别人」）；
//	② 写权限：卷在**请求者自己的**用户卷内 ⇒ 允许；否则需 admin ⇒ 否则 403。
//
// 读只需 ①。返回 (ok=false, status) 时调用方直接回该状态码（并记审计）。
func (h *Handlers) resolveVolumeSettingsAccess(r *http.Request, name string, write bool) (volumeSettingsAccess, int, string) {
	owner := normalizeOwner(ownerFromRequest(r))
	if h.volSet == nil {
		return volumeSettingsAccess{}, http.StatusServiceUnavailable, "卷功能未装配"
	}
	visible := egressVisibleView(volume.AllowedVolumes(h.volSet.All(), owner), owner)
	var vol volume.Volume
	found := false
	for _, v := range visible {
		if v.Name == name {
			vol, found = v, true
			break
		}
	}
	if !found {
		return volumeSettingsAccess{}, http.StatusNotFound, "卷不存在"
	}
	be := h.volSet.External(name)
	if be == nil {
		return volumeSettingsAccess{}, http.StatusNotFound, "卷不存在"
	}
	if !write {
		return volumeSettingsAccess{vol: vol, be: be}, http.StatusOK, ""
	}
	if h.isOwnUserVolume(r, owner, name) {
		return volumeSettingsAccess{vol: vol, be: be}, http.StatusOK, ""
	}
	if roleOfRequest(r) != string(accesskey.RoleAdmin) {
		return volumeSettingsAccess{vol: vol, be: be}, http.StatusForbidden,
			"卷设置仅卷所有者或管理员可修改"
	}
	return volumeSettingsAccess{vol: vol, be: be}, http.StatusOK, ""
}

// isOwnUserVolume 报告 name 是否属于请求者自己的用户卷。
// 用**请求者自己的** store 查询（不做跨 owner 扫描：O(owners) 且会引入新的枚举面）。
func (h *Handlers) isOwnUserVolume(r *http.Request, owner, name string) bool {
	if h.userVolumes == nil {
		return false
	}
	v, err := h.userVolumes.Get(owner, name)
	return err == nil && v != nil
}

// roleOfRequest 返回请求主体角色（空角色按 user 处理，与 requireRole 同口径）。
func roleOfRequest(r *http.Request) string {
	p := PrincipalFrom(r.Context())
	if p == nil || p.Role == "" {
		return string(accesskey.RoleUser)
	}
	return p.Role
}

// auditSettings 记录设置面审计（成功与拒绝都记；Detail 只含分区/条数/摘要，**不含规则全文**）。
func (h *Handlers) auditSettings(ctx context.Context, name, section, result, detail string) {
	h.RecordAudit(ctx, AuditEvent{
		Action:     auditActionVolumeSettingsUpdate,
		ObjectType: "volume",
		Object:     name,
		Result:     result,
		Detail:     detail,
		Volume:     name,
	})
	_ = section
}

// settingsSummary 生成规则载荷的**摘要**（条数 + 前 8 位 sha256），审计用。
func settingsSummary(payload []byte) string {
	if len(payload) == 0 {
		return "空载荷"
	}
	var p struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "载荷非法"
	}
	raw := sha256.Sum256(payload)
	sum := hex.EncodeToString(raw[:])
	if len(sum) > 8 {
		sum = sum[:8]
	}
	return fmt.Sprintf("%d 条规则, 摘要 %s", len(p.Rules), sum)
}

// volumeDetailHandler 处理 GET /api/volumes/{name}/detail：基础信息 + 设置分区 + 能力列表。
func (h *Handlers) volumeDetailHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc, status, msg := h.resolveVolumeSettingsAccess(r, name, false)
	if status != http.StatusOK {
		httpjson.Send(w, status, map[string]any{"success": false, "message": msg})
		return
	}
	usage, state := h.volumeStatusMeta(acc.vol, normalizeOwner(ownerFromRequest(r)))
	caps := []string{}
	settings, ok := h.collectVolumeSettings(r.Context(), acc.be)
	if ok {
		caps = append(caps, "settings")
	}
	httpjson.Send(w, http.StatusOK, map[string]any{
		"volume": map[string]any{
			"name": acc.vol.Name, "type": acc.vol.Type, "mode": string(acc.vol.ACL.Mode),
			"capacity": volumeDisplayCapacity(h.volSet, acc.vol), "usage": usage, "state": state,
		},
		"capabilities": caps,
		"settings":     settings,
	})
}

// volumeSettingsGetHandler 处理 GET /api/volumes/{name}/settings：只返回分区列表。
func (h *Handlers) volumeSettingsGetHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc, status, msg := h.resolveVolumeSettingsAccess(r, name, false)
	if status != http.StatusOK {
		httpjson.Send(w, status, map[string]any{"success": false, "message": msg})
		return
	}
	settings, _ := h.collectVolumeSettings(r.Context(), acc.be)
	httpjson.Send(w, http.StatusOK, map[string]any{"settings": settings})
}

// collectVolumeSettings 取后端设置分区（未实现能力 ⇒ (空列表, false)）。
func (h *Handlers) collectVolumeSettings(ctx context.Context, be registry.ExternalBackend) ([]registry.VolumeSettings, bool) {
	sp, ok := be.(registry.SettingsProvider)
	if !ok {
		return []registry.VolumeSettings{}, false
	}
	sections, err := sp.Settings(ctx)
	if err != nil {
		slog.Warn("volume settings: 读取设置分区失败", "error", err)
		return []registry.VolumeSettings{}, true
	}
	if sections == nil {
		sections = []registry.VolumeSettings{}
	}
	return sections, true
}

// volumeSettingsPutHandler 处理 PUT /api/volumes/{name}/settings/{section}。
func (h *Handlers) volumeSettingsPutHandler(w http.ResponseWriter, r *http.Request) {
	name, section := r.PathValue("name"), r.PathValue("section")
	acc, status, msg := h.resolveVolumeSettingsAccess(r, name, true)
	if status != http.StatusOK {
		h.auditSettings(r.Context(), name, section, "denied", msg)
		httpjson.Send(w, status, map[string]any{"success": false, "message": msg})
		return
	}
	sw, ok := acc.be.(registry.SettingsWriter)
	if !ok {
		h.auditSettings(r.Context(), name, section, "denied", "卷不支持设置写入")
		httpjson.Send(w, http.StatusMethodNotAllowed,
			map[string]any{"success": false, "message": "该卷不支持设置写入"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, volumeSettingsMaxBody)
	payload, rerr := io.ReadAll(r.Body)
	if rerr != nil {
		h.auditSettings(r.Context(), name, section, "failed", "请求体读取失败")
		httpjson.Send(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求体读取失败: " + rerr.Error()})
		return
	}
	if uerr := sw.UpdateSettings(r.Context(), section, payload); uerr != nil {
		code := settingsErrorStatus(uerr)
		h.auditSettings(r.Context(), name, section, "failed", uerr.Error())
		httpjson.Send(w, code, map[string]any{"success": false, "message": uerr.Error()})
		return
	}
	h.auditSettings(r.Context(), name, section, "ok", settingsSummary(payload))
	// 回显新版本（从后端重新读，保证与持久化一致）。
	sections, _ := h.collectVolumeSettings(r.Context(), acc.be)
	version := int64(0)
	for _, s := range sections {
		if s.Key != section {
			continue
		}
		var v struct {
			Version int64 `json:"version"`
		}
		if err := json.Unmarshal(s.Value, &v); err == nil {
			version = v.Version
		}
	}
	httpjson.Send(w, http.StatusOK, map[string]any{"success": true, "version": version})
}

// settingsErrorStatus 把后端错误映射为 HTTP 状态码。
//
// 400 的判据：策略校验类错误（policy 包的错误都以 "policy:" 开头），它们是**用户输入问题**；
// 其余未知错误按 500（不把内部故障说成用户的错）。
func settingsErrorStatus(err error) int {
	switch {
	case errors.Is(err, registry.ErrSettingsVersionConflict):
		return http.StatusConflict
	case errors.Is(err, registry.ErrSettingsSectionUnsupported):
		return http.StatusMethodNotAllowed
	case strings.HasPrefix(err.Error(), "policy:"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// AttachVolumeSettingsStores 把「卷设置持久化后端」注入到支持它的卷后端
// （在 setupStateStore 之后调用：StateStore 晚于卷装配才可用）。
//
// stateStore 为 nil ⇒ 注入 nil：后端据此把分区标为只读（不静默只改内存）。
func AttachVolumeSettingsStores(h *Handlers) {
	if h == nil || h.volSet == nil {
		return
	}
	st := h.stateStore
	store := registry.NewStateSettingsStore(st)
	for _, v := range h.volSet.All() {
		be := h.volSet.External(v.Name)
		if be == nil {
			continue
		}
		setter, ok := be.(registry.SettingsStoreSetter)
		if !ok {
			continue
		}
		if err := setter.SetSettingsStore(store); err != nil {
			slog.Warn("volume settings: 注入持久化后端失败（该卷设置将只读）",
				"volume", v.Name, "error", err)
		}
	}
}
