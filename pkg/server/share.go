// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// share.go 是分享域的 **HTTP 面**（创建/访问/列表/撤销）。存储实现已下沉
// pkg/server/share；本文件只做请求解析、源解析与响应写出。

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/rootfs"
)

func (h *Handlers) createShareHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxShareBodySize)

	req, ok := h.parseShareCreateRequest(w, r)
	if !ok {
		return
	}
	if req.Filename == "" {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := pathguard.ValidateFilePath(req.Filename)
	if err != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	owner, src, ok := h.resolveShareSource(w, r, remotePath, req.Volume)
	if !ok {
		return
	}
	if !h.validateShareSourceFile(w, src) {
		return
	}
	ttl, ok := resolveShareTTL(w, req.TTL)
	if !ok {
		return
	}

	link, err := h.shareStore.Create(shareSpec{
		Filename: req.Filename, TenantID: src.TenantID, Rel: src.Rel, Volume: src.Volume,
		Owner: ActorFrom(r.Context()),
		TTL:   ttl, MaxDownloads: req.MaxDownloads, OneTime: req.OneTime, ReadOnly: req.ReadOnly,
		WatermarkSeed: req.Watermark,
	})
	if err != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "创建分享链接失败"}, http.StatusInternalServerError)
		return
	}

	// 分享创建 = 授权变更——发布 share 事件供订阅者感知（fileEvent 载荷只带
	// action/owner/rel/size，**不含 token/password**，无泄露风险）。
	// size = 分享文件大小；rel 去 user/ 前缀（与事件流既有形态一致）。
	h.eventBus().Publish(files.EventShare, owner, strings.TrimPrefix(src.Rel, "user/"), src.Size)

	// 分享创建 = 授权变更（安全敏感）⇒ 必须审计（object 用 token，detail 记文件名）。
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "share_create", ObjectType: "share", Object: link.Token, Result: AuditResultSuccess,
		Detail: "file=" + link.Filename,
	})

	sendJSONResponse(w, ShareCreateResponse{
		Success:      true,
		Token:        link.Token,
		Filename:     link.Filename,
		CreatedAt:    link.CreatedAt.Format(time.RFC3339),
		ExpiresAt:    link.ExpiresAt.Format(time.RFC3339),
		MaxDownloads: link.MaxDownloads,
		OneTime:      link.OneTime,
		ReadOnly:     link.ReadOnly,
	}, http.StatusOK)
}

// parseShareCreateRequest 解析分享创建请求体并校验 body 完整性（I-3：读满触发 bodyValidator EOF 哈希校验）。
func (h *Handlers) parseShareCreateRequest(w http.ResponseWriter, r *http.Request) (*shareCreateRequest, bool) {
	var req shareCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return nil, false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return nil, false
	}
	return &req, true
}

// resolveShareSource 解析分享创建请求的文件定位：owner 归一化 + **跨卷源定位**
// （本地卷租户根 / 外部卷 FS，含 `secretdata` 加密卷）。
//
// 缺陷修复（2026-10-21）：此前只经 `locateOwnerFile`（**本地卷租户根**）定位 ⇒ 外部卷
// （加密卷/网盘卷/S3）上的文件创建分享恒 404。现在按 `hint`（请求体/查询参数 `volume`）
// 或自动定位（本地卷优先，未命中再扫 owner 视图内外部卷）。
func (h *Handlers) resolveShareSource(w http.ResponseWriter, r *http.Request, remotePath, hint string) (owner string, src *ownerSource, ok bool) {
	owner = normalizeOwner(ownerFromRequest(r))
	tnt0 := h.tenantFor(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", nil, false
	}
	rel, relOK := tnt0.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", nil, false
	}
	if hint == "" {
		hint = r.URL.Query().Get("volume")
	}
	resolved, found := h.resolveOwnerSource(r.Context(), owner, rel, hint)
	if !found || resolved == nil {
		// 文件不在 owner 卷视图内（ACL 收紧/卷未装配）⇒ 404 fail-closed（内容不可经 token 流出）。
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return "", nil, false
	}
	return owner, resolved, true
}

// validateShareSourceFile 校验分享源文件可读（本地卷拒绝符号链接/目录；外部卷在源定位阶段
// 已 Stat 命中且非目录）。
func (h *Handlers) validateShareSourceFile(w http.ResponseWriter, src *ownerSource) bool {
	if src == nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return false
	}
	if src.Local != nil {
		// 符号链接拒绝 + 存在性校验：Lstat 不跟随链接，一次完成两者。
		fi, lstatErr := src.Local.Lstat(src.Rel)
		if lstatErr != nil {
			if os.IsNotExist(lstatErr) {
				sendJSONResponse(w, ShareCreateResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
			} else {
				sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "无法访问文件"}, http.StatusInternalServerError)
			}
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "不支持分享符号链接"}, http.StatusBadRequest)
			return false
		}
		if fi.IsDir() {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "不支持分享目录"}, http.StatusBadRequest)
			return false
		}
		src.Size = fi.Size()
	}
	// 校验可读：立即打开并关闭（分享只存定位，不保留句柄——访问时重新解析，TOCTOU 收敛）。
	f, openErr := src.OpenRead(context.Background())
	if openErr != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "无法访问文件"}, http.StatusInternalServerError)
		return false
	}
	_ = f.Close()
	return true
}

// resolveShareTTL 解析并限制分享 TTL：缺省 24h，超过 maxShareTTL 取上限，非法返回错误回包。
func resolveShareTTL(w http.ResponseWriter, ttlStr string) (time.Duration, bool) {
	ttl := 24 * time.Hour
	if ttlStr != "" {
		d, ttlErr := time.ParseDuration(ttlStr)
		if ttlErr != nil {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "无效的 TTL 格式"}, http.StatusBadRequest)
			return 0, false
		}
		if d <= 0 {
			sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "TTL 必须大于 0"}, http.StatusBadRequest)
			return 0, false
		}
		ttl = min(d, maxShareTTL)
	}
	return ttl, true
}

// accessShareHandler 处理 GET /s/{token}，直接流式传输文件内容。
func (h *Handlers) accessShareHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	src, link, ok := h.resolveShareAccess(w, r, token)
	if !ok {
		return
	}

	// 直接流式传输文件，不暴露文件路径。
	// 加密卷走**用户可见**入口（透明解密）并报**明文**长度——raw Open 会把密文原样下发
	// （评审 P0：分享面静默损坏），密文长度当 Content-Length 还会导致客户端 unexpected EOF。
	// 外部卷（含 secretdata）经 ownerSource.OpenRead（卷 FS 层透明解密），本地卷经
	// openUserVisibleRead —— 两者都是「用户可见」语义（缺陷修复 2026-10-21）。
	f, err := src.OpenRead(r.Context())
	if err != nil {
		http.Error(w, "文件读取失败", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	size := src.Size
	if src.Local != nil {
		// 本地卷以**用户可见长度**为准（加密卷逻辑长度 ≠ 文件系统物理长度）。
		if s2, serr := rootfs.UserVisibleSize(src.Local, src.Rel); serr == nil {
			size = s2
		} else {
			http.Error(w, "文件状态读取失败", http.StatusInternalServerError)
			return
		}
	}
	if size < 0 {
		size = 0
	}

	h.streamShareFile(w, r, token, link, f, size)
}

// resolveShareAccess 校验分享链接有效性并消费（Peek → 源定位 → 文件存在 → Consume）。
// 返回源定位（卷感知）与消费后的 link；失败时已回包，ok=false。
func (h *Handlers) resolveShareAccess(w http.ResponseWriter, r *http.Request, token string) (*ownerSource, *ShareLink, bool) {
	// Peek：先查看链接是否存在且文件有效，不修改状态
	link := h.shareStore.Peek(token)
	if link == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgShareInvalid}, http.StatusNotFound)
		return nil, nil, false
	}
	// 纵深防御：Rel 是存储字段，拒绝越桶引用其它功能桶。
	if !strings.HasPrefix(link.Rel, "user/") {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgShareInvalid}, http.StatusNotFound)
		return nil, nil, false
	}
	src, found := h.resolveShareAccessSource(r, link)
	if !found || src == nil {
		// 源文件已不可见（被删/移走/卷被 ACL 收紧）⇒ 410 fail-closed。
		sendJSONResponse(w, UploadResponse{Success: false, Message: "分享文件已不存在"}, http.StatusGone)
		return nil, nil, false
	}
	// Consume：再消费（递增计数、一次性删除）
	link = h.shareStore.Consume(token)
	if link == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "分享链接已被消费"}, http.StatusConflict)
		return nil, nil, false
	}
	return src, link, true
}

// resolveShareAccessSource 解析分享源的**卷感知**定位：
//   - `link.Volume` 非空（新记录）⇒ **只在该卷**重新定位（ACL 复核 + 文件存在性）；
//   - 否则（旧记录/无卷语义）⇒ 旧行为：默认/本地卷优先（locateOwnerFile），未命中再按视图扫描。
func (h *Handlers) resolveShareAccessSource(r *http.Request, link *ShareLink) (*ownerSource, bool) {
	if link.Volume != "" {
		return h.resolveOwnerSource(r.Context(), link.TenantID, link.Rel, link.Volume)
	}
	if h.volSet == nil {
		// 旧装配路径：唯一根。
		tnt := h.tenantFor(link.TenantID)
		if tnt == nil || tnt.Root() == nil || !strings.HasPrefix(link.Rel, tnt.UserRoot()+"/") {
			return nil, false
		}
		fi, err := tnt.Root().Stat(link.Rel)
		if err != nil || fi.IsDir() {
			return nil, false
		}
		return &ownerSource{TenantID: tnt.ID, Rel: link.Rel, Local: tnt.Root(), Size: fi.Size()}, true
	}
	// 旧记录兼容：本地卷优先，未命中再扫描外部卷（与创建时的自动定位同口径）。
	return h.resolveOwnerSource(r.Context(), link.TenantID, link.Rel, "")
}

// streamShareFile 设置分享响应头并以流式方式传输文件内容（不暴露文件路径）。
func (h *Handlers) streamShareFile(w http.ResponseWriter, r *http.Request, token string, link *ShareLink, f io.Reader, size int64) {
	w.Header().Set(headerContentType, contentTypeOctetStream)
	// 分享只读语义可见（roadmap P2）：仅下载的分享响应带 X-Share-ReadOnly 头。
	if link.ReadOnly {
		w.Header().Set("X-Share-ReadOnly", "true")
	}
	// 分享绑定水印（roadmap P2 残余）：创建时指定 watermark → 响应头可见 +
	// 客户端可经 ?transform=thumb&watermark=<seed> 叠加（防截图外流可追溯）。
	if link.WatermarkSeed != "" {
		w.Header().Set("X-Share-Watermark", link.WatermarkSeed)
	}
	w.Header().Set("Content-Disposition", formatContentDisposition(filepath.Base(link.Rel)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if _, err := copyWithContext(w, f, r.Context()); err != nil {
		h.logger.Warn("分享文件流式传输中断", "token", token, "error", err)
	}
}

// listSharesHandler 处理 GET /api/shares，返回所有分享链接。
func (h *Handlers) listSharesHandler(w http.ResponseWriter, r *http.Request) {
	links := h.shareStore.List(ActorFrom(r.Context()))

	type shareItem struct {
		Token        string `json:"token"`
		Filename     string `json:"filename"`
		Volume       string `json:"volume,omitempty"`
		CreatedAt    string `json:"created_at"`
		ExpiresAt    string `json:"expires_at"`
		MaxDownloads int    `json:"max_downloads"`
		Downloads    int    `json:"downloads"`
		OneTime      bool   `json:"one_time"`
		Expired      bool   `json:"expired"`
	}

	now := time.Now()
	items := make([]shareItem, 0, len(links))
	for _, l := range links {
		expired := now.After(l.ExpiresAt) || (l.MaxDownloads > 0 && l.Downloads >= l.MaxDownloads)
		items = append(items, shareItem{
			Token:        l.Token,
			Filename:     l.Filename,
			Volume:       l.Volume,
			CreatedAt:    l.CreatedAt.Format(time.RFC3339),
			ExpiresAt:    l.ExpiresAt.Format(time.RFC3339),
			MaxDownloads: l.MaxDownloads,
			Downloads:    l.Downloads,
			OneTime:      l.OneTime,
			Expired:      expired,
		})
	}

	sendJSONResponse(w, map[string]any{"shares": items}, http.StatusOK)
}

// revokeShareHandler 处理 DELETE /api/shares/{token}，撤销指定分享链接。
func (h *Handlers) revokeShareHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: "token 不能为空"}, http.StatusBadRequest)
		return
	}

	if err := h.shareStore.Revoke(token, ActorFrom(r.Context())); err != nil {
		sendJSONResponse(w, ShareCreateResponse{Success: false, Message: err.Error()}, http.StatusNotFound)
		return
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "share_revoke", ObjectType: "share", Object: token, Result: AuditResultSuccess,
	})
	sendJSONResponse(w, ShareCreateResponse{Success: true, Message: "分享链接已撤销"}, http.StatusOK)
}

type shareCreateRequest struct {
	Filename string `json:"filename"`
	// Volume 是分享源的卷名（可选）。缺省 = 自动定位（本地卷优先，未命中再扫外部卷）；
	// 多卷存在同名 rel 时应显式指定。外部卷/加密卷（如 secretdata vault）靠它或自动扫描命中。
	Volume       string `json:"volume"`
	TTL          string `json:"ttl"`
	MaxDownloads int    `json:"max_downloads"`
	OneTime      bool   `json:"one_time"`
	ReadOnly     bool   `json:"readonly"`
	Watermark    string `json:"watermark"`
}
