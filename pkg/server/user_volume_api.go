// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// user_volume_api.go 实现用户自有卷管理 API（U3，用户确认）：
//   - POST   /api/volumes/user：创建用户卷（仅外部类型，type 已注册 backend + extra 合法）
//   - GET    /api/volumes/user：列出我的用户卷（owner 过滤）
//   - DELETE /api/volumes/user?name=<n>：删除（运行中引用 → 409；跨 owner → 404）
//
// 用户卷语义（用户 2026-09-17 定案）：
//   - 仅外部类型（baidupcs 等 backend 插件注册的）——本地卷归系统卷（config volumes[]）；
//   - 独立卷容量（UserVolume.Capacity，不计 owner 配额）；
//   - 运行中引用删除 → 409（syncmgr 活跃任务引用检查）；
//   - owner 校验：所有操作以请求主体（ownerFromRequest）为准，跨 owner 404 防枚举。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// errVolumeCycle 是封装卷底层卷成环引用哨兵错误：建卷时沿 target 链向上遍历，若回落到
// 自身/已访问节点 → 400（fail-closed，不落盘）。环的成因是装配期/config 绕过本校验的历史
// 遗留（建卷校验保证 API 自建的链恒无环），本检测兜底拒绝把新卷链入已存在的环。
var errVolumeCycle = errors.New("volume: 底层卷成环引用")

// createUserVolumeRequest 是 POST /api/volumes/user 请求体。
type createUserVolumeRequest struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Capacity int64          `json:"capacity"`
	Extra    map[string]any `json:"extra"`
}

// userVolumesListResponse 是 GET /api/volumes/user 响应。
type userVolumesListResponse struct {
	Volumes []UserVolume `json:"volumes"`
}

// validateWrapperVolumeReq 校验封装卷建卷请求（防成环 fail-closed，不落盘）：
//
//  1. type 的 schema 中 volume-select 必填字段逐一校验 Extra[key] 非空；
//  2. 底层卷必须存在且为**本 owner 可用**（volume.AllowedVolumes 命中，含本地 config 卷
//     与既有外部卷；无权/不存在统一文案不区分——防枚举 + 防跨 owner 封装他人私有卷）；
//  3. AllowWrapper=false 的 volume-select 目标不得再是 wrapper（链必须终止于叶子）；
//  4. 沿 target 链向上遍历（仅遍历 owner 可用的卷），若回落自身/已访问节点 → errVolumeCycle。
//
// 未注册 type / 无 schema（后端未实现 SchemaProvider 且未登记静态 schema）→ 若该类型是
// wrapper category（secretdata/secrets/egress）则兜底强制 target 必填（否则防环空转）；
// 非 wrapper 无 schema → 跳过（type 未注册由后续 NewBackend fail-fast 拦截）。
func validateWrapperVolumeReq(owner, name, typ string, extra map[string]any, vs *registry.Set) error {
	var allowed []volume.Volume
	if vs != nil {
		allowed = volume.AllowedVolumes(vs.All(), owner)
	}
	fields := registry.BackendSchema(typ)
	if len(fields) == 0 && registry.CategoryOf(typ) == "wrapper" {
		// 兜底：wrapper 类型即使未登记 schema 也强制 target 校验（fail-closed 防环）。
		fields = []registry.FieldSchema{{Key: "target", Type: "volume-select", Required: true, AllowWrapper: true}}
	}
	return validateWrapperFields(name, fields, extra, allowed)
}

// validateWrapperFields 逐一校验类型 schema 的 volume-select 必填字段。
func validateWrapperFields(name string, fields []registry.FieldSchema, extra map[string]any, allowed []volume.Volume) error {
	for _, f := range fields {
		if f.Type != "volume-select" || !f.Required {
			continue
		}
		if err := validateWrapperTarget(name, f, extra, allowed); err != nil {
			return err
		}
	}
	return nil
}

// validateWrapperTarget 校验单个 volume-select 字段：Extra 非空 + 底层卷存在且 owner
// 可用 + AllowWrapper 约束 + 防环。
func validateWrapperTarget(name string, f registry.FieldSchema, extra map[string]any, allowed []volume.Volume) error {
	target, _ := extra[f.Key].(string)
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("volume: 底层卷字段 %s 必填", f.Key)
	}
	if !volumeInSet(allowed, target) {
		// 统一文案：不存在 / 无权限不区分（防跨 owner 探测私有卷存在性）。
		return fmt.Errorf("volume: 底层卷 %q 不存在或无权访问", target)
	}
	if !f.AllowWrapper {
		if _, isWrapper := wrapperTargetOfSet(allowed, target); isWrapper {
			return fmt.Errorf("volume: 底层卷 %q 不允许再套封装卷", target)
		}
	}
	return detectWrapperCycle(name, target, allowed)
}

// volumeInSet 判定卷是否在给定（owner 可访问）集合中。
func volumeInSet(vols []volume.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}

// wrapperTargetOfSet 返回 owner 可访问集合中卷的 Extra["target"]（该卷是封装卷且已设底层
// 卷时）；非 wrapper / 无 target / 不在集合（他人私有卷不可见）→ ok=false（链在此终止）。
func wrapperTargetOfSet(vols []volume.Volume, name string) (string, bool) {
	for _, v := range vols {
		if v.Name != name {
			continue
		}
		t, ok := v.Extra["target"].(string)
		return t, ok && strings.TrimSpace(t) != ""
	}
	return "", false
}

// detectWrapperCycle 沿 target 链向上遍历（仅 owner 可访问卷）：从新卷的底层卷 start
// 出发，把新卷名预置为已访问，链上任何节点回落已访问集合 → errVolumeCycle（fail-closed）。
// 链深防御上限 1000（防恶意超长链耗尽 CPU）；上限外同样按环处理。
func detectWrapperCycle(name, start string, allowed []volume.Volume) error {
	visited := map[string]bool{name: true}
	for cur, i := start, 0; i < 1000; i++ {
		if visited[cur] {
			return errVolumeCycle
		}
		visited[cur] = true
		next, ok := wrapperTargetOfSet(allowed, cur)
		if !ok {
			return nil // 叶子/非 wrapper 卷 → 链终止，无环。
		}
		cur = next
	}
	return errVolumeCycle
}

// createUserVolumeHandler 处理 POST /api/volumes/user。
//
// 流程：owner 派生 → 请求体解码 → registry.NewBackend 试构造（type 已注册 + extra 合法，
// fail-fast）→ store.Create 落盘 → Set.AddExternalVolume 注册（失败回滚 store.Delete）。
func (h *Handlers) createUserVolumeHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	if h.userVolumes == nil {
		sendJSONResponse(w, map[string]string{"error": "用户卷功能未装配"}, http.StatusBadRequest)
		return
	}
	var req createUserVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "请求体非法: " + err.Error()}, http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Type == "" {
		sendJSONResponse(w, map[string]string{"error": "name/type 必填"}, http.StatusBadRequest)
		return
	}
	// 封装卷校验（防成环，fail-closed）：schema 中 volume-select 必填字段逐一校验
	// Extra 非空 + 底层卷存在 + AllowWrapper=false 目标非 wrapper + 沿 target 链防环。
	// 不落盘、不注册 Set，失败在 NewBackend 前返回。
	if err := validateWrapperVolumeReq(owner, req.Name, req.Type, req.Extra, h.volSet); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	// 用户卷强制 owner-only ACL（评审 S1，2026-10-06）：新卷注入 Mode=Allow + 单 owner
	// 白名单，使 data 面（列表/下载/wrapper 底层/转存目标）对非 owner fail-closed——此前
	// 注入零值 ACL（Mode==""）令 volume.Authorize 对**任意 owner** 恒 true，跨 owner 枚举
	// /写滥用/封装成链。已存卷（旧 store 文件）零值兼容不静默收紧，仅新卷 owner-only。
	acl := volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{owner: {}}}
	// fail-fast 试构造：type 已注册 + extra 合法（registry.NewBackend 分派）。
	// 失败 → 400（未注册 backend / 凭据缺失等），不落盘。
	v := volume.Volume{Name: req.Name, Type: req.Type, Extra: req.Extra, ACL: acl}
	be, err := registry.NewBackend(r.Context(), v)
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": "type 未注册或 extra 非法: " + err.Error()}, http.StatusBadRequest)
		return
	}
	// store 落盘（重名拒绝）。
	uv := UserVolume{Name: req.Name, Type: req.Type, Capacity: req.Capacity, Extra: req.Extra, ACL: acl}
	if err := h.userVolumes.Create(owner, uv); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusConflict)
		return
	}
	// Set 注册（失败回滚 store.Delete——保持一致）。
	if err := h.volSet.AddExternalVolume(v, be); err != nil {
		_ = h.userVolumes.Delete(owner, req.Name)
		sendJSONResponse(w, map[string]string{"error": "register failed: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]bool{"success": true}, http.StatusOK)
}

// listUserVolumesHandler 处理 GET /api/volumes/user。只列 owner 自己的卷。
func (h *Handlers) listUserVolumesHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	if h.userVolumes == nil {
		sendJSONResponse(w, userVolumesListResponse{Volumes: []UserVolume{}}, http.StatusOK)
		return
	}
	vols, err := h.userVolumes.ListByOwner(owner)
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	// C3：每卷带本系统已用/限额（外部卷容量纳管查询）。
	for i := range vols {
		if h.volSet != nil {
			if be := h.volSet.External(vols[i].Name); be != nil {
				if up, ok := be.(registry.UsageProvider); ok {
					vols[i].Usage = up.Usage()
				}
			}
		}
	}
	sendJSONResponse(w, userVolumesListResponse{Volumes: vols}, http.StatusOK)
}

// deleteUserVolumeHandler 处理 DELETE /api/volumes/user?name=<n>。
//
// 流程：owner 派生 → store.Get（不存在/跨 owner → 404）→ syncmgr 引用检查（活跃任务
// 引用 → 409）→ Set.RemoveExternalVolume（Close 后端）→ store.Delete。
func (h *Handlers) deleteUserVolumeHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	if h.userVolumes == nil {
		sendJSONResponse(w, map[string]string{"error": "用户卷功能未装配"}, http.StatusBadRequest)
		return
	}
	q, _ := url.ParseQuery(r.URL.RawQuery)
	name := q.Get("name")
	if name == "" {
		sendJSONResponse(w, map[string]string{"error": "name 必填"}, http.StatusBadRequest)
		return
	}
	// 存在性 + owner 校验（跨 owner 404 防枚举）。
	v, err := h.userVolumes.Get(owner, name)
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	if v == nil {
		sendJSONResponse(w, map[string]string{"error": "卷不存在"}, http.StatusNotFound)
		return
	}
	// 运行中引用检查（活跃同步任务引用该卷 → 409）。
	if h.syncMgr != nil && h.syncMgr.VolumeInUse(owner, name) {
		sendJSONResponse(w, map[string]string{"error": "卷被活跃同步任务引用，请先取消任务"}, http.StatusConflict)
		return
	}
	// Set 移除（Close 后端）+ store 删文件。
	if err := h.volSet.RemoveExternalVolume(name); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	if err := h.userVolumes.Delete(owner, name); err != nil {
		// Set 已移除但 store 删除失败：回滚 Add（尽力恢复一致性）。保留 ACL（v 来自 store.Get）。
		_ = h.volSet.AddExternalVolume(volume.Volume{Name: name, Type: v.Type, Extra: v.Extra, ACL: v.ACL}, nil)
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]bool{"success": true}, http.StatusOK)
}

// SetUserVolumeStore 注入用户卷 store（装配层 new 后调用；nil 清除，路由返回 400）。
func (h *Handlers) SetUserVolumeStore(s *UserVolumeStore) {
	h.userVolumes = s
}
