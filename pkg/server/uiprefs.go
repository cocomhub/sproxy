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

// UIPrefs 是用户维度的页面配置（JSON 持久化）。
type UIPrefs struct {
	// Delete 是「删除」功能类别的默认行为。
	Delete UIDeletePrefs `json:"delete"`
}

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

// normalize 把零值/非法值归一为**安全默认**（soft）。
func (p UIPrefs) normalize() UIPrefs {
	out := UIPrefs{Delete: UIDeletePrefs{DefaultMode: p.Delete.DefaultMode}}
	switch out.Delete.DefaultMode {
	case UIDeleteModeSoft, UIDeleteModeHard:
	default:
		out.Delete.DefaultMode = UIDeleteModeSoft
	}
	return out
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

// uiPrefsUpdateRequest 是 PUT /api/ui/prefs 的请求体（只更新 `delete.default_mode`）。
type uiPrefsUpdateRequest struct {
	Delete *struct {
		DefaultMode *string `json:"default_mode"`
	} `json:"delete"`
}

// updateUIPrefsHandler 处理 PUT /api/ui/prefs：设置「默认删除方式」。
//
// 校验：`default_mode` ∈ {soft, hard}（大小写不敏感，归一为小写）；其它值 400 不落盘
// （fail-closed——破坏性行为的配置不接受"看不懂就随便存"）。
func (h *Handlers) updateUIPrefsHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	var req uiPrefsUpdateRequest
	if err := decodeSmallJSON(r, &req); err != nil {
		httpjson.SendError(w, http.StatusBadRequest, msgInvalidRequestBody)
		return
	}
	if req.Delete == nil || req.Delete.DefaultMode == nil {
		httpjson.SendError(w, http.StatusBadRequest, "delete.default_mode 不能为空")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(*req.Delete.DefaultMode))
	if mode != UIDeleteModeSoft && mode != UIDeleteModeHard {
		httpjson.SendError(w, http.StatusBadRequest, "delete.default_mode 必须是 soft 或 hard")
		return
	}
	store := h.uiPrefsStoreFor()
	next := store.Load(owner)
	next.Delete.DefaultMode = mode
	if err := store.Save(owner, next); err != nil {
		httpjson.SendError(w, http.StatusInternalServerError, "页面配置保存失败: "+err.Error())
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "ui_prefs_update", ObjectType: "config", Object: "ui/prefs",
		Result: AuditResultSuccess, Detail: "delete.default_mode=" + mode,
	})
	httpjson.Send(w, http.StatusOK, map[string]any{"prefs": next.normalize()})
}

// decodeSmallJSON 解码 ≤4KiB 的 JSON 请求体（页面配置体量固定且很小）。
func decodeSmallJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
