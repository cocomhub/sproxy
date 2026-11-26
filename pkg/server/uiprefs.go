// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// uiprefs.go 是**用户维度的页面配置**（按功能类别分组）：GET/PUT /api/ui/prefs。
//
// 为什么需要（用户 2026-11-26）：页面上的破坏性行为应按「功能类别」配置默认选项，而不是由
// 前端硬编码。首个类别是**删除**：默认删除是否直接硬删——缺省仍是**软删进回收站**（可恢复），
// 只有用户显式把默认改成 hard 才会硬删；回收站内的删除始终是硬删（那是"彻底删除"语义）。
//
// 存储：`<租户根>/meta/ui/prefs.json`（每 owner 一份；随租户卷加密/封印一并生效）。
// 未配置 = 零值 = 全部**安全默认**（安全语义必须是默认值，见 AGENTS.md 7c）。

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/internal/httpjson"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// UIPrefs 是用户维度的页面配置（JSON 持久化）。按**功能类别**分组，每类一个子结构。
type UIPrefs struct {
	// Delete 是「删除」功能类别的默认行为。
	Delete UIDeletePrefs `json:"delete"`
	// Files 是「文件列表」类别的默认视图（排序）。
	Files UIFilesPrefs `json:"files"`
	// Uploads 是「上传」类别的默认目标卷。
	Uploads UIUploadPrefs `json:"uploads"`
}

// UIFilesPrefs 是文件列表类别的配置。
type UIFilesPrefs struct {
	// DefaultSort 是进入文件页时的默认排序列：`name`（缺省）/ `size` / `time`。
	DefaultSort string `json:"default_sort"`
	// DefaultOrder 是默认排序方向：`asc`（缺省）/ `desc`。
	DefaultOrder string `json:"default_order"`
}

// UIUploadPrefs 是上传类别的配置。
type UIUploadPrefs struct {
	// DefaultVolume 是上传时的默认目标卷（缺省 = 自动路由，由服务端按卷策略选）。
	// 卷不存在时页面回落到「全部卷/自动」——配置只是**默认值提示**，不是硬绑定。
	DefaultVolume string `json:"default_volume"`
}

// 文件列表排序取值（稳定字符串，前端与 CLI 共用）。
const (
	UISortName = "name"
	UISortSize = "size"
	UISortTime = "time"

	UIOrderAsc  = "asc"
	UIOrderDesc = "desc"
)

// UIDeletePrefs 是删除类别的配置。
type UIDeletePrefs struct {
	// DefaultMode 是**页面默认删除方式**：`soft`（缺省，进回收站可恢复）/ `hard`（直接删除）。
	// 回收站内的删除恒为硬删，不受本配置影响（"彻底删除"就是彻底删除）。
	DefaultMode string `json:"default_mode"`
}

// 删除默认方式取值（稳定字符串，前端与 CLI 共用）。
const (
	UIDeleteModeSoft = "soft"
	UIDeleteModeHard = "hard"
)

// uiPrefsRel 是配置在租户根内的相对路径。
const uiPrefsRel = "meta/ui/prefs.json"

// normalize 把零值/非法值归一为**安全默认**（删除=soft、排序=name/asc、上传卷=空=自动）。
//
// 所有默认值都是「最安全/最保守」的那一档：软删可恢复、名称升序是稳定视图、不指定卷由
// 服务端策略决定。配置损坏时按本函数归一，绝不因坏配置改变破坏性行为。
func (p UIPrefs) normalize() UIPrefs {
	out := UIPrefs{
		Delete:  UIDeletePrefs{DefaultMode: p.Delete.DefaultMode},
		Files:   UIFilesPrefs{DefaultSort: p.Files.DefaultSort, DefaultOrder: p.Files.DefaultOrder},
		Uploads: UIUploadPrefs{DefaultVolume: p.Uploads.DefaultVolume},
	}
	switch out.Delete.DefaultMode {
	case UIDeleteModeSoft, UIDeleteModeHard:
	default:
		out.Delete.DefaultMode = UIDeleteModeSoft
	}
	switch out.Files.DefaultSort {
	case UISortName, UISortSize, UISortTime:
	default:
		out.Files.DefaultSort = UISortName
	}
	switch out.Files.DefaultOrder {
	case UIOrderAsc, UIOrderDesc:
	default:
		out.Files.DefaultOrder = UIOrderAsc
	}
	out.Uploads.DefaultVolume = normalizeVolumeName(out.Uploads.DefaultVolume)
	return out
}

// normalizeVolumeName 归一「默认卷」：只允许单段卷名（不含分隔符/穿越），非法即空（= 自动）。
//
// 卷名会被直接拼进 `/api/files?volume=` 与上传路由，属**外部输入**：含 `/`、`..`、空字节或
// 空白一律作废（fail-closed 到「自动」），避免把坏配置变成路径拼接风险。
func normalizeVolumeName(name string) string {
	v := strings.TrimSpace(name)
	if v == "" {
		return ""
	}
	if strings.ContainsAny(v, "/\\") || strings.ContainsRune(v, 0) || strings.Contains(v, "..") {
		return ""
	}
	return v
}

// uiPrefsStore 是页面配置读写（每 owner 一份，未配置返回安全默认）。
type uiPrefsStore struct {
	mu sync.Mutex
	// tenantFor 解析 owner 的租户根（owner 不可用 → 返回 nil，按默认处理）。
	tenantFor func(owner string) *storage.Tenant
}

// Load 读取 owner 的配置；文件不存在/损坏/租户不可用 ⇒ **安全默认**（soft），绝不报错阻断删除。
func (s *uiPrefsStore) Load(owner string) UIPrefs {
	def := UIPrefs{}.normalize()
	if s == nil || s.tenantFor == nil {
		return def
	}
	tnt := s.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return def
	}
	raw, err := readRootFileLimited(tnt.Root(), uiPrefsRel, 64<<10)
	if err != nil {
		return def
	}
	var p UIPrefs
	if json.Unmarshal(raw, &p) != nil {
		return def
	}
	return p.normalize()
}

// Save 写入 owner 的配置（原子写：临时文件 + rename）。
func (s *uiPrefsStore) Save(owner string, p UIPrefs) error {
	if s == nil || s.tenantFor == nil {
		return errors.New("页面配置未装配")
	}
	tnt := s.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return errors.New("租户不可用")
	}
	norm := p.normalize()
	raw, err := json.MarshalIndent(norm, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	root := tnt.Root()
	if err := root.MkdirAll(filepath.ToSlash(filepath.Dir(uiPrefsRel)), 0o755); err != nil {
		return err
	}
	abs, ok := root.Abs(uiPrefsRel)
	if !ok {
		return errors.New("页面配置路径非法")
	}
	tmp, terr := os.CreateTemp(filepath.Dir(abs), "prefs-*.json.tmp") // NOSONAR: dir 由已校验的 root 派生
	if terr != nil {
		return terr
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 no-op
	if _, werr := tmp.Write(raw); werr != nil {
		_ = tmp.Close()
		return werr
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		return serr
	}
	if cerr := tmp.Close(); cerr != nil {
		return cerr
	}
	return os.Rename(tmpName, abs)
}

// readRootFileLimited 读 root 内的相对文件（限长，防超大文件 OOM）。
func readRootFileLimited(root *storage.Root, rel string, maxBytes int64) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for int64(len(buf)) < maxBytes {
		n, rerr := f.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	if int64(len(buf)) > maxBytes {
		return nil, errors.New("页面配置文件过大")
	}
	return buf, nil
}

// uiPrefsStoreFor 返回本 Handlers 的页面配置存储（惰性构造，只依赖 tenantFor 闭包）。
func (h *Handlers) uiPrefsStoreFor() *uiPrefsStore {
	h.uiPrefsOnce.Do(func() {
		h.uiPrefs = &uiPrefsStore{tenantFor: h.tenantFor}
	})
	return h.uiPrefs
}

// DeleteDefaultSoft 报告 owner 的「默认删除」是否为软删（页面未显式指定时的行为）。
// 配置损坏/租户不可用一律回落到 **soft**（安全默认）。
func (h *Handlers) DeleteDefaultSoft(owner string) bool {
	p := h.uiPrefsStoreFor().Load(owner)
	return p.Delete.DefaultMode != UIDeleteModeHard
}

// uiPrefsHandler 处理 GET /api/ui/prefs：返回请求者 owner 的页面配置（未配置 = 安全默认）。
func (h *Handlers) uiPrefsHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	p := h.uiPrefsStoreFor().Load(owner)
	httpjson.Send(w, http.StatusOK, map[string]any{
		"prefs": p,
		// defaults 回显服务端的**安全默认**，前端据此标注「未设置」。
		"defaults": UIPrefs{}.normalize(),
	})
}

// uiPrefsUpdateRequest 是 PUT /api/ui/prefs 的请求体：**部分更新**（只改出现的字段）。
type uiPrefsUpdateRequest struct {
	Delete *struct {
		DefaultMode *string `json:"default_mode"`
	} `json:"delete"`
	Files *struct {
		DefaultSort  *string `json:"default_sort"`
		DefaultOrder *string `json:"default_order"`
	} `json:"files"`
	Uploads *struct {
		DefaultVolume *string `json:"default_volume"`
	} `json:"uploads"`
}

// updateUIPrefsHandler 处理 PUT /api/ui/prefs：**部分更新**用户页面配置。
//
// 校验：每个字段都 fail-closed——非法值 400 且**整个请求不落盘**（不会出现"改了一半"）。
// 删除方式属破坏性语义，其余类别是视图默认值；两者都不接受"看不懂就随便存"。
func (h *Handlers) updateUIPrefsHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	var req uiPrefsUpdateRequest
	if err := decodeSmallJSON(r, &req); err != nil {
		httpjson.SendError(w, http.StatusBadRequest, msgInvalidRequestBody)
		return
	}
	store := h.uiPrefsStoreFor()
	next := store.Load(owner)
	var details []string
	var status int
	// 三个类别各自校验（抽成独立方法，保持本函数认知复杂度在阈值内）。
	if next, details, status = applyDeleteCategory(next, req.Delete, details); status != 0 {
		httpjson.SendError(w, status, categoryErrMsg(status))
		return
	}
	if next, details, status = applyFilesCategory(next, req.Files, details); status != 0 {
		httpjson.SendError(w, status, categoryErrMsg(status))
		return
	}
	if next, details, status = applyUploadsCategory(next, req.Uploads, details); status != 0 {
		httpjson.SendError(w, status, categoryErrMsg(status))
		return
	}
	if len(details) == 0 {
		httpjson.SendError(w, http.StatusBadRequest, "没有要更新的字段")
		return
	}
	if err := store.Save(owner, next); err != nil {
		httpjson.SendError(w, http.StatusInternalServerError, "页面配置保存失败: "+err.Error())
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "ui_prefs_update", ObjectType: "config", Object: "ui/prefs",
		Result: AuditResultSuccess, Detail: strings.Join(details, " "),
	})
	httpjson.Send(w, http.StatusOK, map[string]any{"prefs": next.normalize()})
}

// categoryErrMsg 把校验状态码映射为用户可读文案（各类别共用一份，避免重复字面量）。
func categoryErrMsg(status int) string {
	if status == http.StatusBadRequest {
		return msgInvalidUIPrefsField
	}
	return "页面配置保存失败"
}

// msgInvalidUIPrefsField 是页面配置字段非法时的统一文案（各类别细化说明见 docs/api.md）。
const msgInvalidUIPrefsField = "字段取值非法（delete.default_mode ∈ soft|hard；" +
	"files.default_sort ∈ name|size|time；files.default_order ∈ asc|desc；" +
	"uploads.default_volume 必须是合法单段卷名或空串）"

// applyDeleteCategory 应用「删除」类别（未出现 ⇒ 原样返回；非法 ⇒ 返回 400）。
func applyDeleteCategory(next UIPrefs, in *struct {
	DefaultMode *string `json:"default_mode"`
}, details []string) (UIPrefs, []string, int) {
	if in == nil {
		return next, details, 0
	}
	if in.DefaultMode == nil {
		return next, details, http.StatusBadRequest
	}
	mode, ok := parseUIDeleteMode(*in.DefaultMode)
	if !ok {
		return next, details, http.StatusBadRequest
	}
	next.Delete.DefaultMode = mode
	return next, append(details, "delete.default_mode="+mode), 0
}

// applyFilesCategory 应用「文件列表」类别（部分更新：未出现的字段保持原值）。
func applyFilesCategory(next UIPrefs, in *struct {
	DefaultSort  *string `json:"default_sort"`
	DefaultOrder *string `json:"default_order"`
}, details []string) (UIPrefs, []string, int) {
	if in == nil {
		return next, details, 0
	}
	sort, order, ok := applyFilesPrefs(next.Files, in.DefaultSort, in.DefaultOrder)
	if !ok {
		return next, details, http.StatusBadRequest
	}
	next.Files = UIFilesPrefs{DefaultSort: sort, DefaultOrder: order}
	return next, append(details, "files.default_sort="+sort, "files.default_order="+order), 0
}

// applyUploadsCategory 应用「上传」类别（显式空串 = 清除为自动）。
func applyUploadsCategory(next UIPrefs, in *struct {
	DefaultVolume *string `json:"default_volume"`
}, details []string) (UIPrefs, []string, int) {
	if in == nil {
		return next, details, 0
	}
	if in.DefaultVolume == nil {
		return next, details, http.StatusBadRequest
	}
	vol := normalizeVolumeName(*in.DefaultVolume)
	// 显式传了非空值但归一后为空 ⇒ 非法（含分隔符/穿越），不得静默变成"自动"。
	if strings.TrimSpace(*in.DefaultVolume) != "" && vol == "" {
		return next, details, http.StatusBadRequest
	}
	next.Uploads.DefaultVolume = vol
	return next, append(details, "uploads.default_volume="+vol), 0
}

// parseUIDeleteMode 归一删除方式（大小写不敏感）；非法返回 ok=false。
func parseUIDeleteMode(raw string) (string, bool) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode != UIDeleteModeSoft && mode != UIDeleteModeHard {
		return "", false
	}
	return mode, true
}

// applyFilesPrefs 应用排序偏好（部分更新：未出现的字段保持原值）；非法返回 ok=false。
func applyFilesPrefs(cur UIFilesPrefs, sort, order *string) (string, string, bool) {
	nextSort, nextOrder := cur.DefaultSort, cur.DefaultOrder
	if sort != nil {
		v := strings.ToLower(strings.TrimSpace(*sort))
		if v != UISortName && v != UISortSize && v != UISortTime {
			return "", "", false
		}
		nextSort = v
	}
	if order != nil {
		v := strings.ToLower(strings.TrimSpace(*order))
		if v != UIOrderAsc && v != UIOrderDesc {
			return "", "", false
		}
		nextOrder = v
	}
	n := UIPrefs{Files: UIFilesPrefs{DefaultSort: nextSort, DefaultOrder: nextOrder}}.normalize()
	return n.Files.DefaultSort, n.Files.DefaultOrder, true
}

// decodeSmallJSON 解码 ≤4KiB 的 JSON 请求体（页面配置体量固定且很小）。
func decodeSmallJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
