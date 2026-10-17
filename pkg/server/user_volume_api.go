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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
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
	if b, _, ok := ParseNestedTarget(target); ok {
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
		if base, _, nested := ParseNestedTarget(t); nested {
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
	// enclosure 是**内部装配机制**，不是可建卷类型：本字段唯一合法形态是 wrapper 类型
	// （secretdata/secrets…，经 extra.target 声明嵌套封装）。显式拒绝并提示正确写法——
	// 否则会走到 registry.NewBackend 报「类型未注册后端」，客户端无从判断该怎么改。
	if req.Type == enclosure.TypeEnclosure {
		sendJSONResponse(w, map[string]string{
			"error": "enclosure 是内部装配机制，不是可建卷类型；请用 wrapper 类型（如 secretdata/secrets）+ extra.target=\"<底层卷>/<子目录>\" 声明封装",
		}, http.StatusBadRequest)
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
	// 用户卷强制 owner-only ACL（评审 S1，2026-10-06）：新卷注入 Mode=Allow + 单 owner
	// 白名单，使 data 面（列表/下载/wrapper 底层/转存目标）对非 owner fail-closed——此前
	// 注入零值 ACL（Mode==""）令 volume.Authorize 对**任意 owner** 恒 true，跨 owner 枚举
	// /写滥用/封装成链。已存卷（旧 store 文件）零值兼容不静默收紧，仅新卷 owner-only。
	acl := volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{owner: {}}}
	v := volume.Volume{Name: req.Name, Type: req.Type, Capacity: req.Capacity, Extra: req.Extra, ACL: acl}
	// 嵌套封装（`target: <卷>/<子目录>`）翻译出封装声明词汇后走 enclosure 装配路径
	// （方案甲：卷 Type 保持 wrapper 类型，内部机制不出现在卷描述里）：声明校验
	// （含**足迹重叠互斥**）+ 封印注入 + 配额继承 + 后端构造全部由 AttachEnclosureVolume
	// 完成；持久化仍存**原形态**（store 字节零变化）。
	// **owner 贯穿**（2026-10-15）：后端构造在请求内发生 ⇒ 把请求 owner 注入 ctx，使
	// `extra.secret_url: secrets://default-secrets/<name>` 能解析到**本 owner 的**默认桶
	// （per-owner 默认 secrets 卷的「谁创建 = 谁的桶」语义闭环）。
	volCtx := secrets.WithOwner(r.Context(), owner)
	if tv, nested := TranslateNestedVolume(v); nested {
		if err := validateNewEnclosure(volCtx, h.volSet, tv, req.Capacity); err != nil {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, enclosureErrStatus(err))
			return
		}
		uv := UserVolume{Name: req.Name, Type: req.Type, Owner: owner, Capacity: req.Capacity, Extra: req.Extra, ACL: acl}
		if err := h.userVolumes.Create(owner, uv); err != nil {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusConflict)
			return
		}
		if aerr := AttachEnclosureVolume(volCtx, h.volSet, h.logger, h.userVolumes.Root(), tv); aerr != nil {
			_ = h.userVolumes.Delete(owner, req.Name)
			sendJSONResponse(w, map[string]string{"error": "装配失败: " + aerr.Error()}, http.StatusBadRequest)
			return
		}
		sendJSONResponse(w, map[string]bool{"success": true}, http.StatusOK)
		return
	}
	// **用户可创建类型**守卫（M1b）：本地卷**不允许**经建卷 API 创建 —— 本地卷由**配置声明**
	// （`volumes[]` 含 `Root`/加密 key）；若允许，调用方可指定任意 `RootDir` ⇒ **路径/权限面** ✗。
	// 此前该拒绝由 `registry.NewBackend` 硬拒 local 实现；M1b 后 `NewBackend("local")` 可用
	// ⇒ 守卫**前移到此**，保证建卷 API 行为**不变** ✓（空串类型同样拒，与今日一致 ✓）。
	if !registry.IsUserCreatableBackendType(v.Type) {
		sendJSONResponse(w, map[string]string{"error": "type 未注册或 extra 非法: 不支持的类型 " + v.Type}, http.StatusBadRequest)
		return
	}
	// fail-fast 试构造：type 已注册 + extra 合法（registry.NewBackend 分派）。
	// 失败 → 400（未注册 backend / 凭据缺失等），不落盘。
	// Capacity 回填（B3-I1）：委托配额经底层池子 Scope 生效，Volume.Capacity 一并带配额，
	// 使 /api/volumes（卷仪表）与 /api/volumes/user（卷管理）对封装卷显示一致容量——此前
	// 建卷不加 Capacity，仪表读 v.Capacity 恒 0 显示「不限」与实际 507 冲突。
	be, err := registry.NewBackend(volCtx, v)
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": "type 未注册或 extra 非法: " + err.Error()}, http.StatusBadRequest)
		return
	}
	// 卷级容量记账（FS 层）：从**创建期**即包 CapacityFS（此前只有重启 restore 才包，
	// 新建卷在本次进程内 Capacity 不生效——顺手修）。计数器是**唯一实现**（分片日志账本，
	// 建在**原始**卷 FS 上 ⇒ 不自记账、跨进程正确、重启从已占用起算）。
	// F6（用户裁定 2026-10-12）：`capacity:0` 且未显式 unlimited ⇒ **继承已持久化上限**
	// （远端字节仍在）——否则 delete+recreate 写 capacity:0 即解除上限。
	limitPath := capacity.VolumeLimitPath(h.userVolumes.Root(), owner, req.Name)
	limit := capacity.LoadVolumeLimit(limitPath, req.Capacity, req.Unlimited)
	// 用量基线（**恒可用的本地状态**）：卷内账本是 best-effort（卷 FS 可能承载不了 ⇒ 内存降级
	// ⇒ 不持久 ✗），而「delete+recreate 不得归零绕过限额」是**硬要求** ✓ ⇒ 创建时把本地基线
	// **注入**账本（幂等：仅当账本为空时写）✓。账本仍是权威（强制与查询都用它）✓。
	baselineUsed := capacity.LoadVolumeUsed(limitPath)
	be = capacity.WrapStorageVolume(req.Name, be, nil, nil, limit, baselineUsed)
	if lerr := capacity.WriteVolumeState(limitPath, limit, baselineUsed); lerr != nil {
		h.logger.Warn("用户卷状态持久化失败（重建将无法继承上限/用量）", "volume", req.Name, "owner", owner, "error", lerr)
	}
	// store 落盘（重名拒绝）——**store 是运行时用户卷的唯一事实源**（重启由 ScanRestore
	// 恢复；历史 config YAML 写回机制已随阶段 2 迁移移除）。
	// Owner 一并设置（C4 CRITICAL 复测补直）：owner 以参数为准，使本链产出的 UserVolume
	// 同时携带正确 Owner 与 ACL（ACL 是键空间稳定性的载体：owner-only ⇒ 重启后 Shared()
	// 与键空间不变，见 user_volume_store 的 ACL 持久化用例），Owner 字段供任何以
	// UserVolume.Owner 为键的消费方读取。
	// 生效上限（可能来自继承）落元数据（master F6）——否则重启 restore 会用请求里的 0 把
	// 继承的上限抹掉。
	uv := UserVolume{Name: req.Name, Type: req.Type, Owner: owner, Capacity: limit, Extra: req.Extra, ACL: acl}
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

// validateNewEnclosure 建卷前置校验（嵌套封装，无副作用）：① 声明合法性（含足迹重叠互斥）；
// ② 足迹区间可独占（防误删：既有非本卷内容 ⇒ 拒绝，见 enclosure.CheckFootprintRegionsFree）；
// ③ capacity 不超有效源卷池余量（过度承诺的运行期快速失败——即便不校验，父链聚合也会在写入时
// 507，但此处能给出可读的 400）。
func validateNewEnclosure(ctx context.Context, set *registry.Set, tv volume.Volume, capacity int64) error {
	d, decls, err := validateEnclosureDecl(set, tv)
	if err != nil {
		return err
	}
	if rerr := enclosure.CheckFootprintRegionsFree(ctx, set, d, decls); rerr != nil {
		return rerr
	}
	base, _ := enclosure.EffectiveTarget(d, decls)
	if pool := set.Pool(base); pool != nil {
		mx := pool.MaxBytes()
		if mx > 0 && capacity > 0 {
			avail := mx - pool.Usage() - pool.Reserved()
			if capacity > avail {
				return fmt.Errorf("超出底层卷可用配额（capacity=%d > 余量=%d，底层卷 %q）", capacity, avail, base)
			}
		}
	}
	return nil
}

// enclosureErrStatus 把封装声明校验错误映射为 HTTP 状态码：**互斥占用冲突**（足迹重叠/同一路径
// 双声明/**声明与既有内容冲突**，都是「该区间已被占用」）→ 409；其余（缺 source/未注册 wrapper/环/
// 超深/源卷不存在/源卷已加密）→ 400。哨兵由 pkg/volume/enclosure 单一提供，不做字符串匹配。
func enclosureErrStatus(err error) int {
	switch {
	case errors.Is(err, enclosure.ErrFootprintOverlap),
		errors.Is(err, enclosure.ErrConflictClaim),
		errors.Is(err, enclosure.ErrRegionOccupied):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
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
	//
	// **来源统一**（2026-10-17）：经 `volumeUsage` **单一查询面**（配额层 → 容量池 → 后端
	// `UsageProvider`）—— 此前本列表自持一份 `UsageProvider` 覆盖（与 `/api/volumes` 仪表
	// 双链 ⇒ **多漂移面**），已收敛为唯一实现（门禁 R30 钉住）。
	for i := range vols {
		if h.volSet != nil {
			vols[i].Usage = h.volumeUsage(volume.Volume{Name: vols[i].Name, Type: vols[i].Type})
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
//nolint:gocognit // S3776: 用户卷删除装配流程（master F9 flush + 过渡层关联/子目录清理，2026-10）；阶段 2 迁 enclosure 后拆分
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
	// 嵌套封装引用检查（阶段 2 迁移：声明即事实源）：本卷若被其它封装声明的 `source` 直接
	// 引用（`<本卷>:<子路径>`）→ 409（先删引用方，防占用悬空）。查「声明的 Source 指向它」
	// 取代过渡层的 links 注册表——声明集合本身就是权威（config/store 即事实源）。
	if refs := h.enclosureRefsOf(name); len(refs) > 0 {
		sendJSONResponse(w, map[string]string{"error": "卷是封装卷的底层卷（存在嵌套封装引用: " + strings.Join(refs, ", ") + "），请先删除引用它的封装卷"}, http.StatusConflict)
		return
	}
	// 先删 store 元数据（失败则 Set 不变，无需回滚），再摘除运行时卷，最后清计数文件。
	if err := h.userVolumes.Delete(owner, name); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	// 用量基线快照（**本地状态，恒可用**）：账本可能因卷 FS 能力而内存降级（不持久 ✗）⇒
	// 「delete+recreate 不得归零绕过限额」必须由本地状态兜住（设计 §3.6.3）✓。
	if lerr := capacity.WriteVolumeState(
		capacity.VolumeLimitPath(h.userVolumes.Root(), owner, name),
		v.Capacity, h.volumeUsage(volumeFromUserVolume(*v))); lerr != nil {
		h.logger.Warn("用户卷删除：用量基线持久化失败（重建将无法继承用量）",
			"volume", name, "owner", owner, "error", lerr)
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
	// 删除成功：若本卷是嵌套封装卷（用户面 target 语法），拆除底座侧的落地物——回收源卷封印
	// （否则源区间永久只读）、归还并摘除继承容量池、擦除独占区间内容（阶段 2 迁移：取代过渡层
	// 的 clearWrapperLinks/clearWrapperQuota/cleanupWrapperBaseDir）。
	if tv, nested := TranslateNestedVolume(volumeFromUserVolume(*v)); nested {
		DetachEnclosureVolume(h.volSet, h.logger, tv)
	}
	sendJSONResponse(w, map[string]bool{"success": true}, http.StatusOK)
}

// enclosureRefsOf 返回在**当前声明集合**中以 name 作为直接 source（底层卷）的封装卷名
// （删底层卷前查引用 → 409）。声明即事实源：不需要任何运行期注册表。
func (h *Handlers) enclosureRefsOf(name string) []string {
	if h == nil || h.volSet == nil {
		return nil
	}
	decls, err := enclosure.Parse(h.volSet.All())
	if err != nil {
		return nil // 声明集合当前非法（运维侧问题）：不阻塞删除（另有装配期 fail-closed 兜底）
	}
	var out []string
	for _, d := range decls {
		if d.Source == name {
			out = append(out, d.Vol)
		}
	}
	return out
}

// SetUserVolumeStore 注入用户卷 store（装配层 new 后调用；nil 清除，路由返回 400）。
func (h *Handlers) SetUserVolumeStore(s *UserVolumeStore) {
	h.userVolumes = s
}
