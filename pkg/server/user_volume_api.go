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
	"path/filepath"
	"slices"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// errVolumeCycle 是封装卷底层卷成环引用哨兵错误：建卷时沿 target 链向上遍历，若回落到
// 自身/已访问节点 → 400（fail-closed，不落盘）。环的成因是装配期/config 绕过本校验的历史
// 遗留（建卷校验保证 API 自建的链恒无环），本检测兜底拒绝把新卷链入已存在的环。
var errVolumeCycle = errors.New("volume: 底层卷成环引用")

// createUserVolumeRequest 是 POST /api/volumes/user 请求体。
type createUserVolumeRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Capacity int64  `json:"capacity"`
	// Unlimited 显式声明「不限容量」（F6）：`capacity:0` 默认含义是**未声明**（沿用同名卷
	// 已持久化的上限，防 delete+recreate 把上限变不限）；确需不限必须显式置本字段。
	Unlimited bool           `json:"unlimited"`
	Extra     map[string]any `json:"extra"`
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
// 可用 + AllowWrapper 约束 + 防环。target 支持两种形态：
//   - 裸卷名（如 `main`）；
//   - 嵌套封装 `卷名/新子目录`（如 `main/videos`，用户 2026-10-06 确认语义）——取卷名段
//     校验存在性/防环，子目录段的互斥占用与「须不存在」由 validateNestedWrapperTarget 负责。
func validateWrapperTarget(name string, f registry.FieldSchema, extra map[string]any, allowed []volume.Volume) error {
	target, _ := extra[f.Key].(string)
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("volume: 底层卷字段 %s 必填", f.Key)
	}
	// 嵌套形态：校验对象为卷名段（子目录不是卷）。
	base := target
	if b, _, ok := volume.SplitNestedTarget(target); ok {
		base = b
	}
	if !volumeInSet(allowed, base) {
		// 统一文案：不存在 / 无权限不区分（防跨 owner 探测私有卷存在性）。
		return fmt.Errorf("volume: 底层卷 %q 不存在或无权访问", base)
	}
	if !f.AllowWrapper {
		if _, isWrapper := wrapperTargetOfSet(allowed, base); isWrapper {
			return fmt.Errorf("volume: 底层卷 %q 不允许再套封装卷", base)
		}
	}
	return detectWrapperCycle(name, base, allowed)
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
// 嵌套 target（`卷/子目录`）归一为卷名段（子目录不是卷，链上节点以卷为单位判环）。
func wrapperTargetOfSet(vols []volume.Volume, name string) (string, bool) {
	for _, v := range vols {
		if v.Name != name {
			continue
		}
		t, ok := v.Extra["target"].(string)
		if !ok || strings.TrimSpace(t) == "" {
			return "", false
		}
		if base, _, nested := volume.SplitNestedTarget(t); nested {
			return base, true
		}
		return t, true
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
//
//nolint:gocognit // S3776: 用户卷建卷装配流程（master F7/F6 校验 + 过渡层封装卷校验/ACL/配额委托，2026-10）；阶段 2 迁 enclosure 后拆分
func (h *Handlers) createUserVolumeHandler(w http.ResponseWriter, r *http.Request) { // NOSONAR: S3776 — 同理由
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
	// F7（评审 P3）：**先校验卷名再拼路径/IO**——下方 counterPath 由 req.Name 拼接，而真正的
	// 卷名校验此前只在 store.Create 内（更晚）。名字含分隔符/`..` 时会读到 meta 目录之外的
	// `*.capacity`（且读失败直接 500），顺序与错误码都不当。此处与 store 用**同一判定**
	// （storage.ValidSegmentName），提前使路径安全不依赖下游副作用。
	if !storage.ValidSegmentName(req.Name) {
		sendJSONResponse(w, map[string]string{"error": "卷名非法"}, http.StatusBadRequest)
		return
	}
	// 封装卷校验（防成环，fail-closed）：schema 中 volume-select 必填字段逐一校验
	// Extra 非空 + 底层卷存在 + AllowWrapper=false 目标非 wrapper + 沿 target 链防环。
	// 不落盘、不注册 Set，失败在 NewBackend 前返回。
	if err := validateWrapperVolumeReq(owner, req.Name, req.Type, req.Extra, h.volSet); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	// 嵌套封装 target（`<卷>/<子目录>`）额外校验：互斥占用 + 子目录必须不存在（409）。
	// 非嵌套 target（local+root 等）零回归。
	if err := h.validateNestedWrapperTarget(r.Context(), req.Extra); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusConflict)
		return
	}
	// 配额委托容量预检（方案B，2026-10-06）：封装卷设 capacity 须 ≤ 底层卷池余量
	// （超出 → 400「超出底层卷可用配额」；不设则 Scope 0 不限、由底层卷反向管理）。
	if err := h.validateWrapperQuotaCapacity(req.Extra, req.Capacity); err != nil {
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
	// Capacity 回填（B3-I1）：委托配额经底层池子 Scope 生效，Volume.Capacity 一并带配额，
	// 使 /api/volumes（卷仪表）与 /api/volumes/user（卷管理）对封装卷显示一致容量——此前
	// 建卷不加 Capacity，仪表读 v.Capacity 恒 0 显示「不限」与实际 507 冲突。
	v := volume.Volume{Name: req.Name, Type: req.Type, Capacity: req.Capacity, Extra: req.Extra, ACL: acl}
	be, err := registry.NewBackend(r.Context(), v)
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": "type 未注册或 extra 非法: " + err.Error()}, http.StatusBadRequest)
		return
	}
	// 卷级容量记账（FS 层）：从**创建期**即包 CapacityFS（此前只有重启 restore 才包，
	// 新建卷在本次进程内 Capacity 不生效——顺手修）。计数器持久化到与 restore 相同的
	// 路径 <root>/<owner>/meta/volume/<name>.capacity（**非 .json**：避开 UserVolumeStore
	// 的 *.json 扫描，防计数文件被误当卷元数据）；重启后 Load 续用。
	counterPath := filepath.Join(h.userVolumes.Root(), owner, "meta", "volume", req.Name+".capacity")
	// F6：`capacity:0` 且未显式 unlimited ⇒ **继承同名卷已持久化的上限**（删除用户卷时刻意
	// 保留 `.capacity`，因为远端字节仍在）——否则 delete+recreate 写 capacity:0 即解除上限。
	counter, lerr := capacity.LoadForCreate(counterPath, req.Capacity, req.Unlimited)
	if lerr != nil {
		sendJSONResponse(w, map[string]string{"error": "容量快照读取失败: " + lerr.Error()}, http.StatusInternalServerError)
		return
	}
	if counter.CorruptSnapshot() {
		h.logger.Warn("用户卷容量快照损坏，已重置计数（从零累计）", "volume", req.Name, "owner", owner, "path", counterPath)
	}
	be = capacity.WrapBackend(be, counter)
	// store 落盘（重名拒绝）。
	// Owner 一并设置（C4 CRITICAL 复测补直）：owner 以参数为准，使本链产出的 UserVolume
	// 同时携带正确 Owner 与 ACL。config 写回（下方 AppendVolume(uv)）已改以 ACL.Owners 为
	// 准（见 config_writer.volumeEntryNode），Owner 字段是冗余保底（store.Create 内部亦按
	// owner 参数回填，二者一致），供任何以 UserVolume.Owner 为键的消费方读取不改。
	// 生效上限（可能来自继承）落元数据（master F6）——否则重启 restore 会用请求里的 0 把
	// 继承的上限抹掉。
	uv := UserVolume{Name: req.Name, Type: req.Type, Owner: owner, Capacity: counter.Capacity(), Extra: req.Extra, ACL: acl}
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
	// 创建成功后登记嵌套封装占用关联（互斥占用生命周期：删底层卷查引用 409、删封装卷清关联）。
	// register 内做互斥重叠判定（防并发建卷重叠子目录 TOCTOU，fail-close）：冲突 → 回滚
	// 已建卷（Set 移除 + store 删除，尽力一致），返回占用 409。
	if !h.registerWrapperLink(owner, req.Name, req.Extra) {
		_ = h.volSet.RemoveExternalVolume(req.Name)
		_ = h.userVolumes.Delete(owner, req.Name)
		sendJSONResponse(w, map[string]string{"error": errVolumeDirOccupied.Error()}, http.StatusConflict)
		return
	}
	// 配额委托挂载（方案B）：在底层卷容量池挂占用子目录 Scope（capacity 已前置校验 ≤ 底层
	// 余量，此处失败仅底层池不可用/重复委托等防御路径）→ 回滚已建卷 + 清关联，尽力一致。
	if err := h.registerWrapperQuota(req.Name, req.Extra, req.Capacity); err != nil {
		_ = h.volSet.RemoveExternalVolume(req.Name)
		_ = h.userVolumes.Delete(owner, req.Name)
		h.clearWrapperLinks(req.Name)
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	// C4 config 写回（2026-10-06 用户裁决）：创建成功后把卷追加到 config volumes 段，
	// 重启后卷不丢。写回失败 → 回滚已建卷（Set/store/links/配额），保持一致性。
	if h.configWriter != nil {
		if err := h.configWriter.AppendVolume(uv); err != nil {
			h.logger.Error("用户卷创建：config 写回失败，回滚建卷", "volume", req.Name, "owner", owner, "error", err)
			_ = h.volSet.RemoveExternalVolume(req.Name)
			_ = h.userVolumes.Delete(owner, req.Name)
			h.clearWrapperLinks(req.Name)
			h.clearWrapperQuota(req.Name)
			sendJSONResponse(w, map[string]string{"error": "卷已创建但 config 写回失败，已回滚（config 文件不可写或格式非法）"}, http.StatusInternalServerError)
			return
		}
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
	// 来源说明（评审 B4 Minor #5）：/api/volumes/user 的 usage 取后端 UsageProvider（外部卷
	// 计数），/api/volumes（卷仪表）取容量池 Pool.Usage()（委托子 Scope）——两条链对封装卷
	// 来源不同（仪表读委托池、本列表读后端计数，后端非 UsageProvider 时 0）。当前 UI 卷管理
	// 列表不渲染 usage 列、仪表/进度条只消费 /api/volumes，无用户影响；将来在本列表加 usage
	// 列须先统一来源（封装卷读 Pool.Usage() 与仪表一致）。
	for i := range vols {
		if h.volSet != nil {
			if be := h.volSet.External(vols[i].Name); be != nil {
				if up, ok := be.(registry.UsageProvider); ok {
					vols[i].Usage = up.Usage()
				}
			}
		}
		// 敏感信息防护（D-CRITICAL 修复）：store.Get 已把 ExtraEnc 解密回填 Extra——
		// 直接序列化会把凭据（bduss/stoken/access_key/secret_key 等）明文回传给浏览器。
		// 列表响应只暴露非敏感配置（root/capacity/binary_path），凭据键剔除。
		vols[i].Extra = redactVolumeExtra(vols[i].Extra)
	}
	sendJSONResponse(w, userVolumesListResponse{Volumes: vols}, http.StatusOK)
}

// sensitiveVolumeExtraKeys 是卷 Extra 中的凭据/敏感键（回显时剔除）。
var sensitiveVolumeExtraKeys = []string{
	"bduss", "stoken", "ptoken", "cookies", "access_key_secret", "secret_key",
	"access_key", "client_secret", "password", "token",
}

// redactVolumeExtra 剔除卷 Extra 中的凭据键（保留配置键如 root/local_root/binary_path/capacity）。
// 防御纵深：即使 store 未加密（masterKey 为 nil 旧装配）也不把凭据回传。
func redactVolumeExtra(extra map[string]any) map[string]any {
	if len(extra) == 0 {
		return extra
	}
	out := make(map[string]any, len(extra))
	for k, v := range extra {
		if isSensitiveVolumeKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// isSensitiveVolumeKey 判定卷 Extra 键是否为凭据（含 key/secret/password/token 形态）。
func isSensitiveVolumeKey(k string) bool {
	lk := strings.ToLower(k)
	if slices.Contains(sensitiveVolumeExtraKeys, lk) {
		return true
	}
	return strings.Contains(lk, "secret") || strings.Contains(lk, "password") ||
		strings.Contains(lk, "bduss") || strings.Contains(lk, "stoken") ||
		strings.Contains(lk, "access_key")
}

// deleteUserVolumeHandler 处理 DELETE /api/volumes/user?name=<n>。
//
// 流程：owner 派生 → store.Get（不存在/跨 owner → 404）→ syncmgr 引用检查（活跃任务
// 引用 → 409）→ Set.RemoveExternalVolume（Close 后端）→ store.Delete。
//
//nolint:gocognit // S3776: 用户卷删除装配流程（master F9 flush + 过渡层 config 写回/关联清理，2026-10）；阶段 2 迁 enclosure 后拆分
func (h *Handlers) deleteUserVolumeHandler(w http.ResponseWriter, r *http.Request) { // NOSONAR: S3776 — 同理由
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
	// 嵌套封装关联：本卷是其它封装卷的底层且仍有引用 → 409（先删引用方，防占用悬空）。
	if h.links().hasRefs(name) {
		sendJSONResponse(w, map[string]string{"error": "卷是封装卷的底层卷（存在嵌套封装引用），请先删除引用它的封装卷"}, http.StatusConflict)
		return
	}
	// I-4（评审，2026-10-07）：config 写回**先行**（任何删除副作用前）——失败即终止删除
	// （500，卷原样保留）。否则「store 已删 + config 写回失败」→ config 残留使已删封装卷
	// 重启复活并重占子目录（僵尸卷 + 409 阻塞重建），运维须手工清 config 行。config 先行
	// 保证「删除成功 ⇒ config 已一致移除」；其后任一步失败卷仍在 store → 重启由
	// restoreUserVolumes 恢复（store 是权威，非僵尸；RemoveVolume 对未声明卷 no-op）。
	if h.configWriter != nil {
		if err := h.configWriter.RemoveVolume(name); err != nil {
			h.logger.Error("用户卷删除：config 写回失败，删除终止（卷保留）", "volume", name, "owner", owner, "error", err)
			sendJSONResponse(w, map[string]string{"error": "删除终止：config 文件不可写或格式非法（卷保留，请修复 config 后重试）"}, http.StatusInternalServerError)
			return
		}
	}
	// 先删 store 元数据（失败则 Set 不变，无需回滚），再摘除运行时卷，最后清计数文件。
	if err := h.userVolumes.Delete(owner, name); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	// F9：摘除前把计数快照 flush（`Release` 是节流的）——否则本次进程对该卷的删除量丢失，
	// 同名重建时 used 偏高（伪 507）。best-effort + 可观测。
	if be := h.volSet.External(name); be != nil {
		if fl, ok := be.(interface{ Flush() error }); ok {
			if ferr := fl.Flush(); ferr != nil {
				h.logger.Warn("用户卷摘除：容量计数快照落盘失败", "volume", name, "owner", owner, "error", ferr)
			}
		}
	}
	if err := h.volSet.RemoveExternalVolume(name); err != nil {
		// store 已删但 Set 摘除失败：回滚重建 store 元数据（Set 未变，保持一致）。
		_ = h.userVolumes.Create(owner, *v)
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	// **不删容量计数文件**：卷删除仅移除元数据/句柄，远端已写入的字节仍在（本系统不删
	// 远端内容）；保留 used 使同名重建时 Load 到旧占用（否则 delete+recreate 把上限变成
	// 「每轮限额 × 轮数」，绕过「Σ占用 ≤ 卷限额」）。计数文件为安全保留，非泄漏。
	// 删除成功：若本卷是封装卷（曾占用底层子目录），清除其占用关联与配额委托（生命周期闭环）
	// + 清理占用子目录残留（C5：配额归还底层池 + 重建同目录不 409；I-③：先整流删除卷内
	// 数据再删目录——带数据封装卷不留密文残留、子目录可复用）。
	h.clearWrapperLinks(name)
	h.clearWrapperQuota(name)
	h.cleanupWrapperBaseDir(v.Extra)
	sendJSONResponse(w, map[string]bool{"success": true}, http.StatusOK)
}

// SetUserVolumeStore 注入用户卷 store（装配层 new 后调用；nil 清除，路由返回 400）。
func (h *Handlers) SetUserVolumeStore(s *UserVolumeStore) {
	h.userVolumes = s
}

// SetConfigWriter 注入 config 写回能力（C4，2026-10-06）：/api/volumes/user 创建/删除后
// 把卷写回 config 文件 volumes 段（重启不丢）。nil = 不写回（零回归）。
func (h *Handlers) SetConfigWriter(w ConfigWriter) {
	h.configWriter = w
}
