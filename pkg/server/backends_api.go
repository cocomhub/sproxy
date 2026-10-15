// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_api.go 实现 backend 列表 API（V4）：GET /api/backends 返回已注册卷后端类型 +
// per-type schema（registry.BackendSchemas()），供 Web UI「我的用户卷」创建表单 type 下拉
// 动态感知 + schema 驱动字段渲染——未来任何新 backend（S3/…）只 RegisterBackend 注册
// （并按需实现 SchemaProvider）即自动出现在前端，无需改前端代码。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

type backendsListResponse struct {
	Backends []registry.BackendSchemaInfo `json:"backends"`
}

// backendsHandler 处理 GET /api/backends。返回 registry 已注册后端类型的
// type→(category, fields) schema 列表（动态）。Label 留空缺省（前端按 type 显示）；
// 未实现 SchemaProvider 的类型 fields 为空数组。不依赖 volSet（注册表是包级状态，
// 与卷集合装配无关）——未装配卷集合也返回已注册类型。
func (h *Handlers) backendsHandler(w http.ResponseWriter, r *http.Request) {
	sendJSONResponse(w, backendsListResponse{Backends: registry.BackendSchemas()}, http.StatusOK)
}

// backendPresignHandler 处理 POST /api/backends/{type}/presign?path=&method=&expires=：
// 按 type 从注册表构造临时后端 → 断言 Presigner → 返回签名 URL（签名 v4 直传）。
// 未注册 type → 404；后端不支持 Presign → 405；缺 path / method 非法 → 400。
func (h *Handlers) backendPresignHandler(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	owner := normalizeOwner(ownerFromRequest(r))
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
	// 占用写保护（评审 F2 首要闭合点）：PUT 直传由客户端对对象存储发起，服务端**看不到那次写**
	// ⇒ 占用判定只能在签发期做（complete 的登记拦截虽有纵深，但那时「只读」目录里已经躺了一个
	// 未登记、不计配额的游离对象）。GET 不拦——占用目录是只读而非不可读。
	if method == http.MethodPut {
		if oerr := h.presignOccupiedGuard(typ, owner, path); oerr != nil {
			http.Error(w, oerr.Error(), http.StatusForbidden)
			return
		}
	}
	be, err := h.presignBackendFor(r.Context(), typ, owner)
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
func (h *Handlers) presignBackendFor(ctx context.Context, typ, owner string) (registry.ExternalBackend, error) {
	if !registry.IsBackendRegistered(typ) {
		return nil, errPresignTypeUnregistered
	}
	if v, ok, err := h.presignVolumeFor(typ, owner); err != nil {
		return nil, err
	} else if ok {
		return registry.NewBackend(ctx, v)
	}
	return registry.NewBackend(ctx, volume.Volume{Type: typ})
}

// presignVolumeFor 解析该类型下**已配置且授权给 owner** 的卷（预签名与登记的记账/ACL 依据）。
// 返回 (卷, true, nil) = 命中；该类型无已配置卷 = (零值, false, nil)；有卷但 ACL 拒绝 =
// (零值, false, errPresignForbidden)。
func (h *Handlers) presignVolumeFor(typ, owner string) (volume.Volume, bool, error) {
	if h.volSet == nil {
		return volume.Volume{}, false, nil
	}
	matched := false
	for _, v := range h.volSet.All() {
		if v.Type != typ {
			continue
		}
		matched = true
		if !v.Authorize(owner) {
			continue // 该卷不授权该 owner：不得借其凭证直传（评审 P2：presign 缺卷 ACL 校验）
		}
		return v, true, nil
	}
	if matched {
		return volume.Volume{}, false, errPresignForbidden
	}
	return volume.Volume{}, false, nil
}

// errPresignTypeUnregistered 是后端类型未注册的哨兵（调用方据此映射 404）。
var errPresignTypeUnregistered = errors.New("后端类型未注册")

// errPresignForbidden 表示该类型存在已配置卷、但调用方对该卷无 ACL 授权（→ 403）。
var errPresignForbidden = errors.New("后端类型对当前用户不可用（卷 ACL 拒绝）")

// presignBackendError 分档映射：未注册 → 404；ACL 拒绝 → 403；其余构造失败 → 500。
func (h *Handlers) presignBackendError(w http.ResponseWriter, typ string, err error) {
	switch {
	case errors.Is(err, errPresignTypeUnregistered):
		http.Error(w, "后端类型未注册: "+typ, http.StatusNotFound)
	case errors.Is(err, errPresignForbidden):
		http.Error(w, "后端类型对当前用户不可用（卷 ACL 拒绝）", http.StatusForbidden)
	default:
		http.Error(w, "后端构造失败（检查卷配置）: "+err.Error(), http.StatusInternalServerError)
	}
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
	// 类型未注册 → 404（先于参数校验：契约「未注册类型 → 404」不得被 400 抢先）。
	if !registry.IsBackendRegistered(typ) {
		http.Error(w, "后端类型未注册: "+typ, http.StatusNotFound)
		return
	}
	// 2026-10-14 P0（评审 §158 记档）：直传对象必须**登记**——否则 presign 是容量/配额旁路
	// （对象对 owner 配额与卷容量池完全不可见，且无 sidecar/台账）。客户端须声明 size 与
	// sha256（服务端无法在直传后廉价读取对象内容）；缺任一/非法 ⇒ 400（不静默跳过登记）。
	owner := normalizeOwner(ownerFromRequest(r))
	size, sizeErr := presignSizeParam(r)
	sha := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sha256")))
	if sizeErr != nil || !files.IsSHA256Hex(sha) {
		http.Error(w, "登记需携带 size（非负整数）与 sha256（64 位十六进制）", http.StatusBadRequest)
		return
	}
	vol, matched, verr := h.presignVolumeFor(typ, owner)
	if verr != nil {
		h.presignBackendError(w, typ, verr)
		return
	}
	if matched {
		// path 形态 = `<owner>/user/<rel>`（presignOwnerKey 保证桶为 user）⇒ RemotePath 取
		// **user 桶相对路径**（与 HTTP 上传/远程写面同一约定：Tenant.UserRel 会补 user/ 前缀）。
		rel := presignRemoteRel(owner, path)
		if _, rerr := h.fileService().RegisterDirectUpload(r.Context(), files.DirectUploadInput{
			Owner: owner, RemotePath: rel, Volume: vol.Name, Size: size, SHA256: sha,
		}); rerr != nil {
			h.presignRegisterError(w, rerr)
			return
		}
		sendJSONResponse(w, map[string]string{"ok": "registered"}, http.StatusOK)
		return
	}
	// 无已配置卷（裸后端：纯内存/测试）：无卷语义可登记（不存在容量旁路）⇒ 沿用存在性确认。
	be, err := h.presignBackendFor(r.Context(), typ, owner)
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

// presignOccupiedGuard 在**签发期**对「被封装卷占用的底层子目录」fail-closed 拒绝（评审 F2）：
// presign 的 PUT URL 由客户端直传对象存储，服务端看不到那次写 ⇒ 只能在签发时拦。
//
// 判定与登记侧（complete → RegisterDirectUpload → routeUploadForTarget → checkWrapperOccupiedWrite）
// **同源同坐标**：命中的卷是「该 type 解析出的已配置卷」，坐标是卷内**用户可见相对路径**
// （presignRemoteRel 剥掉 `<owner>/user/` 前缀，正是占用 Subdir 的口径）。
// 无已配置卷（裸后端）或该类型对调用方不可用（ACL 拒绝）时返回 nil——后者由
// presignBackendError 统一分档 403，不得在此抢先改变错误码。
func (h *Handlers) presignOccupiedGuard(typ, owner, path string) error {
	vol, matched, err := h.presignVolumeFor(typ, owner)
	if err != nil || !matched {
		return nil
	}
	return h.checkWrapperOccupiedWrite(vol.Name, presignRemoteRel(owner, path))
}

// presignRemoteRel 从收敛后的预签名路径（`<owner>/user/<rel>`）取出**用户桶相对路径**
// （无 `user/` 前缀）——签发期占用判定（presignOccupiedGuard）与登记（backendPresignCompleteHandler
// → DirectUploadInput.RemotePath）共用同一映射，防两处键口径漂移。
func presignRemoteRel(owner, path string) string {
	rel, _ := strings.CutPrefix(strings.TrimPrefix(path, owner+"/"), "user/")
	return rel
}

// presignSizeParam 解析登记用的 size（非负整数，必填）。
func presignSizeParam(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("size"))
	if raw == "" {
		return 0, errors.New("缺少 size")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("size 需为非负整数")
	}
	return n, nil
}

// presignRegisterError 把域侧登记错误映射为 HTTP：*files.HTTPError 自带状态码与文案
// （400 参数 / 403 ACL / 404 对象不存在 / 409 size 不符 / 507 配额不足），其余 500。
func (h *Handlers) presignRegisterError(w http.ResponseWriter, err error) {
	if he, ok := errors.AsType[*files.HTTPError](err); ok {
		http.Error(w, he.Message, he.Status)
		return
	}
	http.Error(w, "直传登记失败: "+err.Error(), http.StatusInternalServerError)
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
