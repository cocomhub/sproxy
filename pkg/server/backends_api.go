// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_api.go 实现 backend 列表 API（V4）：GET /api/backends 返回已注册卷后端类型
// （registry.BackendTypes()），供 Web UI「我的用户卷」创建表单 type 下拉动态感知——
// 未来任何新 backend（S3/…）只 RegisterBackend 注册即自动出现在前端，无需改前端代码。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

type backendsListResponse struct {
	Backends []string `json:"backends"`
}

// backendsHandler 处理 GET /api/backends。返回 registry 已注册后端类型列表（动态）。
// 不依赖 volSet（注册表是包级状态，与卷集合装配无关）——未装配卷集合也返回已注册类型。
func (h *Handlers) backendsHandler(w http.ResponseWriter, r *http.Request) {
	sendJSONResponse(w, backendsListResponse{Backends: registry.BackendTypes()}, http.StatusOK)
}

// backendPresignHandler 处理 POST /api/backends/{type}/presign?path=&method=&expires=：
// 按 type 从注册表构造临时后端 → 断言 Presigner → 返回签名 URL（签名 v4 直传）。
// 未注册 type → 404；后端不支持 Presign → 405；缺 path / method 非法 → 400。
func (h *Handlers) backendPresignHandler(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	path, perr := h.presignPathFor(r)
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusForbidden)
		return
	}
	method := strings.ToUpper(r.URL.Query().Get("method"))
	if method != http.MethodPut && method != http.MethodGet {
		http.Error(w, "method 需为 PUT 或 GET", http.StatusBadRequest)
		return
	}
	expires := int64(0)
	if es := r.URL.Query().Get("expires"); es != "" {
		n, err := strconv.ParseInt(es, 10, 64)
		if err != nil || n <= 0 {
			http.Error(w, "expires 需为正整数秒", http.StatusBadRequest)
			return
		}
		expires = n
	}
	be, err := h.presignBackendFor(r.Context(), typ)
	if err != nil {
		h.presignBackendError(w, typ, err)
		return
	}
	defer be.Close()
	p, ok := be.(registry.Presigner)
	if !ok {
		http.Error(w, "后端不支持预签名直传: "+typ, http.StatusMethodNotAllowed)
		return
	}
	u, err := p.PresignedURL(r.Context(), path, method, expires)
	if err != nil {
		if errors.Is(err, syncpkg.ErrUnsupported) {
			http.Error(w, "后端不支持预签名直传: "+typ, http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "预签名失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]string{"url": u}, http.StatusOK)
}

// presignBackendFor 解析预签名目标后端：**优先取已配置的该类型卷**（携带凭证 Extra）
// ——直接用 `volume.Volume{Type: typ}` 构造对 s3/baidupcs 等一切需配置的后端必失败
// （真实类型被错报 404「未注册」，生产上直传端点实际不可用）。无配置卷时回落按 type
// 构造（纯内存/测试后端可用）。
func (h *Handlers) presignBackendFor(ctx context.Context, typ string) (registry.ExternalBackend, error) {
	if !registry.IsBackendRegistered(typ) {
		return nil, errPresignTypeUnregistered
	}
	if h.volSet != nil {
		for _, v := range h.volSet.All() {
			if v.Type == typ {
				return registry.NewBackend(ctx, v)
			}
		}
	}
	return registry.NewBackend(ctx, volume.Volume{Type: typ})
}

// errPresignTypeUnregistered 是后端类型未注册的哨兵（调用方据此映射 404）。
var errPresignTypeUnregistered = errors.New("后端类型未注册")

// presignBackendError 分档映射：未注册 → 404；已注册但构造失败（缺配置/远端不可达）→ 500。
func (h *Handlers) presignBackendError(w http.ResponseWriter, typ string, err error) {
	if errors.Is(err, errPresignTypeUnregistered) {
		http.Error(w, "后端类型未注册: "+typ, http.StatusNotFound)
		return
	}
	http.Error(w, "后端构造失败（检查卷配置）: "+err.Error(), http.StatusInternalServerError)
}

// backendPresignCompleteHandler 处理 POST /api/backends/{type}/presign/complete?path=：
// 客户端直传完成后登记——服务端确认对象已存在（sync.FS.Stat）。
// 未注册 type → 404；对象不存在 → 404（fail-closed）。
func (h *Handlers) backendPresignCompleteHandler(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	path, perr := h.presignPathFor(r)
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusForbidden)
		return
	}
	be, err := h.presignBackendFor(r.Context(), typ)
	if err != nil {
		h.presignBackendError(w, typ, err)
		return
	}
	defer be.Close()
	raw := be.FS()
	if raw == nil {
		http.Error(w, "后端无文件视图", http.StatusInternalServerError)
		return
	}
	fs := trusted.Guard(raw)
	// 全仓 Stat 契约为「不存在返回 (nil, nil)」——必须判 e == nil（旧实现只看 err==nil，
	// 对象不存在也回 200 registered = 假确认；P2 对抗评审）。
	if e, err := fs.Stat(r.Context(), path); err != nil || e == nil {
		http.Error(w, "对象不存在（直传未完成或路径错误）", http.StatusNotFound)
		return
	}
	sendJSONResponse(w, map[string]string{"ok": "registered"}, http.StatusOK)
}

// guardPresignPath 校验并归一预签名/登记路径：拒绝路径穿越/绝对路径/空值，且拒绝
// meta 等内部功能桶（P2-2/F3：预签名直接签发对象键、不经 MetaBucketGuard 的
// keyspace，必须在此拦死——否则客户端可 PUT/GET 到 `<owner>/meta/...` 侧车/凭据）。
func guardPresignPath(raw string) (string, error) {
	clean, err := pathguard.ValidateFilePath(raw)
	if err != nil {
		return "", err
	}
	if bucket, _, ok := volume.BucketOf(clean); ok && bucket == "meta" {
		return "", fmt.Errorf("路径命中内部功能桶，禁止直传")
	}
	return clean, nil
}

// errPresignCrossOwner / errPresignNonUserBucket 是预签名越权哨兵（达 403）。
var (
	errPresignCrossOwner    = errors.New("越权：预签名路径不属于当前主体")
	errPresignNonUserBucket = errors.New("预签名仅允许 user 桶")
)

// presignPathFor 解析请求的卷内路径并**收敛到调用者的 owner 键空间**。
//
// 安全背景（第 5 轮对抗评审 P0，**预存缺陷**）：原实现把客户端给的裸路径直接交给
// 后端 `PresignedURL`——后端键空间恒为 `<归一 owner>/<bucket>/<path>`
// （`volume.Location.FSPath`），故任意已认证用户可对 `<他人 owner>/user/...` 取
// 服务端凭据签名的 PUT/GET URL（跨租户读写）。此处服务端统一重绑：
//   - 裸相对路径（`a.txt`）→ `<owner>/user/a.txt`；
//   - `user/a.txt` → `<owner>/user/a.txt`；
//   - `<owner>/user/a.txt` → 原样（owner 必须为调用者）；
//   - `<他人 owner>/...` → 403；非 user 桶（cloud/version/…）→ 403。
//
// 这样对象落点与文件 API（`externalUploadSink.ownerKey` → `FSPath`）一致——此前
// 直传对象落在裸 key、文件 API 根本看不到。
func (h *Handlers) presignPathFor(r *http.Request) (string, error) {
	clean, err := guardPresignPath(r.URL.Query().Get("path"))
	if err != nil {
		return "", err
	}
	return presignOwnerKey(normalizeOwner(ownerFromRequest(r)), clean)
}

// presignOwnerKey 把已清洗的卷内路径收敛为 `<owner>/user/<rel>`（纯函数，便于测试）。
func presignOwnerKey(owner, clean string) (string, error) {
	owner = normalizeOwner(owner)
	seg := strings.Split(clean, "/")
	// 共享卷形态 `<owner>/<bucket>/<rel...>`：次段为保留桶名。
	if len(seg) >= 2 && storage.IsReservedBucketName(seg[1]) {
		if seg[0] != owner {
			return "", errPresignCrossOwner
		}
		seg = seg[1:]
	}
	if !storage.IsReservedBucketName(seg[0]) {
		// 裸相对路径（无桶段）→ 视为调用者 user 桶内路径。
		return owner + "/user/" + clean, nil
	}
	if strings.ToLower(seg[0]) != "user" {
		return "", errPresignNonUserBucket
	}
	rel := strings.Join(seg[1:], "/")
	if rel == "" {
		return "", errors.New("预签名路径不能为 user 桶根")
	}
	return owner + "/user/" + rel, nil
}
