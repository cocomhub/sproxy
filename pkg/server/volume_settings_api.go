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
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/internal/httpjson"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/state"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// auditActionVolumeSettingsUpdate 是「写卷设置」的业务审计动作（R35-b 登记项）。
const auditActionVolumeSettingsUpdate = "volume_settings_update"

// 业务审计结果取值（**必须**用既有契约 success/denied/error）：审计控制台、/api/audit?result=
// 筛选与通知规则都按这套值判定；自造 ok/failed 会让成功事件显示成红「失败」且筛不到。
const (
	auditResultSuccess = "success"
	auditResultDenied  = "denied"
	auditResultError   = "error"
)

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
	// **be 可以为 nil**：本地卷（默认卷）没有外部后端，但它是可见卷、卷表里有「详情」按钮
	// ⇒ 必须 200 + 空 settings（「此卷暂无可调配置」），不能报「卷不存在」。
	be := h.volSet.External(name)
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
// 分区名并入 Object（`<卷名>/<分区>`），避免为它单开一个不存在的审计字段。
func (h *Handlers) auditSettings(ctx context.Context, name, section, result, detail string) {
	h.RecordAudit(ctx, AuditEvent{
		Action:     auditActionVolumeSettingsUpdate,
		ObjectType: "volume",
		Object:     name + "/" + section,
		Result:     result,
		Detail:     detail,
		Volume:     name,
	})
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
	h.ensureSettingsStoreInjected(name, acc.be) // 运行期新建卷的惰性补注入
	caps := []string{}
	settings, ok, serr := h.collectVolumeSettings(r.Context(), acc.be)
	if serr != nil {
		// 读取失败与「没有可调配置」必须可区分：把服务端故障显示成「暂无可调配置」会让
		// 运维以为卷型不支持，从而彻底放弃排查。
		slog.Warn("volume settings: 读取设置分区失败", "volume", name, "error", serr)
		httpjson.Send(w, http.StatusInternalServerError,
			map[string]any{"success": false, "message": "读取卷设置失败（服务端内部错误，详见服务端日志）"})
		return
	}
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
	h.ensureSettingsStoreInjected(name, acc.be)
	settings, _, serr := h.collectVolumeSettings(r.Context(), acc.be)
	if serr != nil {
		slog.Warn("volume settings: 读取设置分区失败", "volume", name, "error", serr)
		httpjson.Send(w, http.StatusInternalServerError,
			map[string]any{"success": false, "message": "读取卷设置失败（服务端内部错误，详见服务端日志）"})
		return
	}
	httpjson.Send(w, http.StatusOK, map[string]any{"settings": settings})
}

// collectVolumeSettings 取后端设置分区。
//
// 三态：`ErrUnsupported`（包装器表达「inner 没这个能力」）⇒ (空列表, false, nil)；
// 其它错误 ⇒ (空列表, true, err) 让调用方回 5xx——**不能**把服务端故障伪装成「没有配置」。
func (h *Handlers) collectVolumeSettings(ctx context.Context, be registry.ExternalBackend) ([]registry.VolumeSettings, bool, error) {
	sp, ok := be.(registry.SettingsProvider)
	if !ok {
		return []registry.VolumeSettings{}, false, nil
	}
	sections, err := sp.Settings(ctx)
	if err != nil {
		if errors.Is(err, syncpkg.ErrUnsupported) {
			return []registry.VolumeSettings{}, false, nil
		}
		return []registry.VolumeSettings{}, true, err
	}
	if sections == nil {
		sections = []registry.VolumeSettings{}
	}
	return sections, true, nil
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
	h.ensureSettingsStoreInjected(name, acc.be) // 运行期新建卷：首次保存前补注入
	sw, ok := acc.be.(registry.SettingsWriter)
	if !ok {
		h.auditSettings(r.Context(), name, section, auditResultDenied, "卷不支持设置写入")
		httpjson.Send(w, http.StatusMethodNotAllowed,
			map[string]any{"success": false, "message": "该卷不支持设置写入"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, volumeSettingsMaxBody)
	payload, rerr := io.ReadAll(r.Body)
	if rerr != nil {
		// 超限（MaxBytesReader）也走 400：属于客户端输入问题，且不回内部细节。
		h.auditSettings(r.Context(), name, section, auditResultError, "请求体读取失败")
		httpjson.Send(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求体读取失败（超限或非法）"})
		return
	}
	if uerr := sw.UpdateSettings(r.Context(), section, payload); uerr != nil {
		code := settingsErrorStatus(uerr)
		h.auditSettings(r.Context(), name, section, auditResultError, safeSettingsError(uerr))
		// 客户端错误回原文（含规则下标，便于精确定位）；服务端内部错误只回通用文案
		// （CLAUDE.md：不把原始 error 抛给客户端，可能含路径/驱动细节）。
		msg := uerr.Error()
		if code >= http.StatusInternalServerError {
			msg = "保存失败（服务端内部错误，详见服务端日志）"
			slog.Warn("volume settings: 写入失败", "volume", name, "section", section, "error", uerr)
		}
		httpjson.Send(w, code, map[string]any{"success": false, "message": msg})
		return
	}
	h.auditSettings(r.Context(), name, section, auditResultSuccess, settingsSummary(payload))
	// 回显新版本（从后端重新读，保证与持久化一致）。
	sections, _, _ := h.collectVolumeSettings(r.Context(), acc.be)
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
	case errors.Is(err, registry.ErrSettingsPayloadInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// safeSettingsError 生成**可入审计**的错误摘要：客户端输入类错误保留原因（便于追责），
// 服务端内部错误只留类别。
//
// 为什么不能直接落 err.Error()：正则编译错误的文本**内嵌原始表达式**（Go 的 regexp.Error
// 含 Expr）⇒ 会把用户配置的路径模式写进审计，违背「审计不含规则全文」的约束。
func safeSettingsError(err error) string {
	switch settingsErrorStatus(err) {
	case http.StatusBadRequest, http.StatusConflict, http.StatusMethodNotAllowed:
		return truncateRunes(err.Error(), 200)
	default:
		return "内部错误"
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
	if h.stateStore == nil {
		// 默认单机部署（state_store.type 缺省/local 且非集群）不会装配 StateStore，但
		// LocalStateStore 本就实现了 Get/Put/CAS ⇒ 用**轻量本地实例**兜底，让「卷设置
		// 持久化」在默认部署下也成立。**不调 SetStateStore**：那会把分享/索引也切到
		// StateStore 后端（更大的行为变更，不属于本功能）。
		h.volumeSettingsLocalStore = state.NewLocalStateStore(h.volumeSettingsStateDir(), nil)
	}
	for _, v := range h.volSet.All() {
		be := h.volSet.External(v.Name)
		if be == nil {
			continue
		}
		h.ensureSettingsStoreInjected(v.Name, be)
	}
}

// volumeSettingsStateDir 返回卷设置本地兜底存储目录（`<存储根>/state/volume-settings`）。
// 与 StateStore 的 `<存储根>/state` 同根不同子目录：即便将来启用 StateStore 也不会互相干扰。
func (h *Handlers) volumeSettingsStateDir() string {
	root := ""
	if h.cfgPtr != nil {
		if cfg := h.cfgPtr.Load(); cfg != nil {
			root = cfg.StorageRoot
		}
	}
	if root == "" {
		root = "."
	}
	return filepath.Join(root, "state", "volume-settings")
}

// settingsStoreInjected 记录「已注入过持久化后端的后端」。
//
// 键 = `<卷名> + 后端实例指针`：只按卷名会让「卷被删除后重建」（新后端实例）复用旧的
// 「已注入」标记而**跳过注入**（该卷设置永久只读）。包级 map 在测试间也会串味。
var settingsStoreInjected sync.Map

// forgetVolumeSettings 在**卷被删除**时清掉它的持久化设置与注入缓存。
//
// 为什么必须清（安全审查）：设置键只按卷名（`volume_policy/<卷名>`），若删除后残留，**同名
// 重建**的卷会静默继承旧规则——规则含路径语义，属跨身份残留状态（旧卷主可能仍能用规则影响
// 新卷主的文件布局）。与「容量计数文件保留」不同：本功能是**配置**而非占用事实，重建应从
// 默认/基线开始。
func (h *Handlers) forgetVolumeSettings(ctx context.Context, name string) {
	if store := registry.NewStateSettingsStore(h.settingsStateStore()); store != nil {
		if err := store.Delete(ctx, name); err != nil {
			slog.Warn("volume settings: 删除卷设置失败（同名重建可能继承旧规则）", "volume", name, "error", err)
		}
	}
	// 注入缓存按「卷名+后端实例」为键：逐条清掉该卷名下的全部条目。
	prefix := name + "|"
	settingsStoreInjected.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && strings.HasPrefix(key, prefix) {
			settingsStoreInjected.Delete(k)
		}
		return true
	})
}

// volumeSettingsRefresh 让支持持久化注入的后端**重新载入**卷设置（多实例收敛入口）。
//
// 为什么挂在 refresh 上（审查 P1-2）：多实例共享 StateStore 时，A 保存规则后 B 仍用旧规则写
// ⇒ 同卷不同实例产出不同块布局（读不出错，但确定性/秒传命中率下降）。`POST /api/volumes/refresh`
// 是全仓既有的运维收敛通道（本进程外改动收敛），在这里顺带重载规则不需要新 API、不需要新 CLI。
//
// 未实现 SettingsStoreSetter 的卷 ⇒ no-op（nil）。
func (h *Handlers) volumeSettingsRefresh(ctx context.Context, name string) error {
	be := h.volSet.External(name)
	if be == nil {
		return nil
	}
	// 优先走**可返回错误**的显式重载能力：Setter 在装配期失败只告警（不能阻断挂载），
	// 若 refresh 复用它做重载，收敛失败时仍会回「已重载」成功回执（审查 Important）。
	if rl, ok := be.(registry.SettingsReloader); ok {
		if err := rl.ReloadSettings(ctx); err != nil {
			return fmt.Errorf("重载卷设置失败: %w", err)
		}
		slog.Info("volume settings: refresh 触发规则重载", "volume", name)
		return nil
	}
	// 兼容：只实现 Setter 的后端（没有严格重载语义）——保留原行为，但调用方不再据它判定成功。
	if setter, ok := be.(registry.SettingsStoreSetter); ok {
		if err := setter.SetSettingsStore(registry.NewStateSettingsStore(h.settingsStateStore())); err != nil {
			return fmt.Errorf("重载卷设置失败: %w", err)
		}
		slog.Info("volume settings: refresh 触发规则重载（Setter 兼容路径）", "volume", name)
	}
	return nil
}

// settingsStateStore 返回卷设置用的持久化后端：优先装配好的 StateStore，其次本地兜底。
//
// **必须显式判 nil 再赋值**（不能 `st := h.stateStore; if st == nil { st = h.volumeSettingsLocalStore }`）：
// 把 nil 的 `*state.LocalStateStore` 赋给 `state.StateStore` 接口会得到**非 nil 接口**
// （typed-nil），`NewStateSettingsStore` 的 `st == nil` 判空失效 ⇒ 包出一个调用即 panic 的实现。
func (h *Handlers) settingsStateStore() state.StateStore {
	if h.stateStore != nil {
		return h.stateStore
	}
	if h.volumeSettingsLocalStore != nil {
		return h.volumeSettingsLocalStore
	}
	return nil
}

// settingsInjectKey 生成注入缓存的键（卷名 + 后端实例地址）。
func settingsInjectKey(name string, be registry.ExternalBackend) string {
	return fmt.Sprintf("%s|%p", name, be)
}

// ensureSettingsStoreInjected 幂等地给支持注入的后端装上持久化后端。
//
// 为什么需要惰性路径：`AttachVolumeSettingsStores` 在启动时跑一次，而运行期经
// `POST /api/volumes/user` 新建的卷在那之后才存在 ⇒ 若不补注入，用户新建卷打开详情
// 永远是只读，直到重启进程。
func (h *Handlers) ensureSettingsStoreInjected(name string, be registry.ExternalBackend) {
	if be == nil {
		return
	}
	setter, ok := be.(registry.SettingsStoreSetter)
	if !ok {
		return
	}
	key := settingsInjectKey(name, be)
	if _, done := settingsStoreInjected.Load(key); done {
		return
	}
	if err := setter.SetSettingsStore(registry.NewStateSettingsStore(h.settingsStateStore())); err != nil {
		slog.Warn("volume settings: 注入持久化后端失败（该卷设置将只读）", "volume", name, "error", err)
		return
	}
	settingsStoreInjected.Store(key, struct{}{})
}
