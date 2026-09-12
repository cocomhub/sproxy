// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// downloadKindCloudArchive 是云任务归档下载的 kind 值。
// kind 白名单仅此一项——归档是用户主动打包的产出（归档名含随机难枚举），
// 按 owner 隔离存储（租户 archive 桶 <root>/<tenant>/archive/）。
const downloadKindCloudArchive = "cloud_archive"

// cloudArchivePathFor 解析 kind=cloud_archive 归档在请求者租户 archive 桶下的相对路径。
// 返回 (tenant, rel)（rel 形如 "archive/<name>"）；租户不可用或名称未通过 FeatureRel
// 校验（单文件名已由 validateCloudArchiveName 前置校验，此处纵深防御）时返回 (nil, "")。
func (h *Handlers) cloudArchivePathFor(r *http.Request, name string) (*storage.Tenant, string) {
	tnt := h.tenantFor(ownerFromRequest(r))
	if tnt == nil {
		return nil, ""
	}
	rel, ok := tnt.FeatureRel("archive", name)
	if !ok {
		return nil, ""
	}
	return tnt, rel
}

// downloadKindCloudTask 是云任务文件下载的 kind 值。
// 云任务文件按任务 owner 落租户 cloud 桶（<root>/<tenant>/cloud/<taskID>/<file>），
// filename 传 <taskID>/<file>，服务端校验任务属于当前 owner 后经 FeatureRel("cloud", ...) 解析。
const downloadKindCloudTask = "cloud_task"

// downloadPathError 携带 HTTP 状态码与消息，统一 /download 与 /download/chunk 的路径解析错误。
type downloadPathError struct {
	status  int
	message string
}

func (e *downloadPathError) Error() string { return e.message }

// validateCloudArchiveName 校验 kind=cloud_archive 的归档名。
// 归档名必须是单文件名（无路径分隔符），拒绝空、绝对路径、..、Windows 非法字符。
// 与 ValidateFilePath 的规则对齐，但不含 .__ 首段拒绝——归档名经 FeatureRel("archive", name)
// 解析到租户 archive 功能桶（服务端内部构造，用户不可直达），服务端负责拼接与防穿越。
func validateCloudArchiveName(name string) *downloadPathError {
	invalid := &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidFilename}
	name = strings.TrimSpace(name)
	if name == "" {
		return &downloadPathError{status: http.StatusBadRequest, message: errMsgEmptyFilename}
	}
	if strings.ContainsRune(name, 0) {
		return invalid
	}
	if name[0] == '/' || name[0] == '\\' {
		return invalid
	}
	// 拒绝路径分隔符（/ 与 \）：归档名必须是单文件名，服务端拼接归档目录。
	if strings.ContainsAny(name, `/\`) {
		return invalid
	}
	// 拒绝 . 与 ..；filepath.Base 兜底，防 Windows 盘符/尾分隔符等绕过。
	if filepath.Clean(name) == "." || filepath.Clean(name) == ".." || filepath.Base(name) != name {
		return invalid
	}
	// Windows 非法字符（对齐 ValidateFilePath）。
	if runtime.GOOS == "windows" {
		const invalidChars = `<>:"|?*`
		if strings.ContainsAny(name, invalidChars) {
			return invalid
		}
	}
	return nil
}

// downloadPath 是 resolveDownloadPath 的解析结果。
// 所有 kind 均返回 tnt + 租户根内相对路径 rel，后续经 tenant.Root().Open/Stat 打开
// （os.Root 防符号链接逃逸）：
//   - kind 为空 → user/<path>
//   - kind=cloud_task → cloud/<taskID>/<file>
//   - kind=cloud_archive → archive/<name>
type downloadPath struct {
	filename string          // 用户可见文件名（Content-Disposition / 日志 / checksum key）
	tnt      *storage.Tenant // 非 nil = 经 Tenant.Root 定位（防符号链接逃逸）
	rel      string          // 租户根内相对路径（如 user/dir/f.txt、cloud/<taskID>/<file>、archive/<name>）
}

// resolveDownloadPath 解析 /download、/download/chunk 与 /api/files/stat 的文件路径。
// 返回 downloadPath：
//
//   - kind 为空 → 普通下载：ValidateFilePath 校验 + Tenant.UserRel 映射到 user 桶，
//     返回租户与根内相对路径（后续经 tenant.Root().Open/Stat 打开）。租户不可用或
//     UserRel 拒绝（.__/__ 内部前缀 / 非法段名）→ 400（downloadPathError）。
//   - kind=cloud_archive → 归档名（单文件名），按请求者租户 FeatureRel("archive", name)
//     解析到 archive 桶，经 tenant.Root().Open/Stat 打开。
//   - kind=cloud_task → 云任务文件（任务归属校验）：校验任务对请求者可见 + 请求文件精确
//     匹配任务声明文件名后，按任务 owner 租户 FeatureRel("cloud", <taskID>/<file>) 解析 rel，
//     经 tenant.Root().Open/Stat 打开。
//   - 其它 kind → 400（白名单，防任意内部目录访问）。
func (h *Handlers) resolveDownloadPath(r *http.Request) (*downloadPath, error) {
	name := r.URL.Query().Get("filename")
	kind := r.URL.Query().Get("kind")

	switch kind {
	case "":
		remotePath, vErr := pathguard.ValidateFilePath(name)
		if vErr != nil {
			if name == "" {
				return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgEmptyFilename}
			}
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidFilename}
		}
		// 读取侧守卫（审查 #4 收敛 + 审查 I-1）：普通下载/stat 不得访问非 user 桶路径。
		// UserRel 内部：NormalizeRemote + 逐段 ValidSegmentName（拒绝 .__ 内部前缀、
		// Windows 保留设备名等）+ 首段 __ 遗留前缀拒绝；功能桶名首段合法（user/ 桶内）。
		// 路径映射与卷无关（user/<path> 相对各卷租户根），用默认租户做纯路径校验。
		owner := normalizeOwner(ownerFromRequest(r))
		tnt0 := h.tenantFor(owner)
		if tnt0 == nil {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		rel, ok := tnt0.UserRel(remotePath)
		if !ok {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		// 跨卷定位（任务 5）：默认卷快路径命中即用；未命中遍历视图其余卷。带显式 ?volume=
		// 只在指定卷定位（不在视图/卷上无此文件 → 404，fail-closed 不泄卷存在性）。
		explicitVol := r.URL.Query().Get("volume")
		loc, found := h.locateForRead(owner, rel, explicitVol)
		if !found {
			if explicitVol != "" {
				return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
			}
			// 全视图未命中：仅当默认卷对 owner 授权才回落默认租户（由调用方 Open/Stat 产出
			// 404/500，与单卷既有错误语义一致）。默认卷被 ACL 排除时不得回落——否则 owner 可经
			// 默认租户 Open 读到默认卷自身路径的遗留文件（ACL bypass，AD-6）。
			if !h.defaultVolumeAllows(owner) {
				return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
			}
			return &downloadPath{filename: remotePath, tnt: tnt0, rel: rel}, nil
		}
		return &downloadPath{filename: remotePath, tnt: loc.tenant, rel: rel}, nil
	case downloadKindCloudArchive:
		if aErr := validateCloudArchiveName(name); aErr != nil {
			return nil, aErr
		}
		tnt, rel := h.cloudArchivePathFor(r, name)
		if tnt == nil || rel == "" {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		return &downloadPath{filename: name, tnt: tnt, rel: rel}, nil
	case downloadKindCloudTask:
		remotePath, vErr := pathguard.ValidateFilePath(name)
		if vErr != nil {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidFilename}
		}
		// 校验任务属于当前 owner + 请求文件 == task.Filename（审查 I3：只允许下载
		// 任务声明的原始文件，防下载任务目录下 .partial/.partial.etag 等残留）。
		// 跨租户任务或文件不匹配 → 404（防枚举，不泄露存在性）。
		// 格式必须为 <taskID>/<file>（含分隔符），否则视为不存在。
		slash := strings.IndexByte(remotePath, '/')
		if slash <= 0 {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		taskID := remotePath[:slash]
		if h.cloudMgr == nil {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		task, ok := h.cloudMgr.SnapshotTask(taskID, ownerFromRequest(r))
		if !ok {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		// 仅允许 taskID/<task.Filename> 精确匹配（防下载任务目录下其它文件）。
		if remotePath != taskID+"/"+task.Filename {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		// 按任务 owner 租户解析 cloud 桶 rel（文件按任务 owner 落盘；空 owner 任务在
		// anonymous 租户）。用 task.Owner 而非 ownerFromRequest(r)：空 owner（管理员/未认证）
		// 请求者对任意可见任务都能下载（可见性已由 SnapshotTask 保证），且空 owner 任务对
		// 认证用户可见时文件在 anonymous 租户——按请求者解析会导致 404（行为回归）。
		tnt := h.tenantFor(task.Owner)
		if tnt == nil {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		rel, ok := tnt.FeatureRel("cloud", remotePath)
		if !ok {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		return &downloadPath{filename: remotePath, tnt: tnt, rel: rel}, nil
	default:
		return nil, &downloadPathError{
			status:  http.StatusBadRequest,
			message: "未知下载 kind: " + kind,
		}
	}
}

// ---- 路由注册引用的薄适配（路由 pattern 与处理器名逐字不变） ----
//
// 实体在 `pkg/files/read.go`。本文件保留装配层独有的那部分：**路径解析**
// （resolveDownloadPath：kind 白名单 / 跨卷读定位 / 云任务归属校验 / 归档名单文件名校验），
// 经接缝 Deps.ResolveDownloadPath 交给领域包消费；错误类型 *downloadPathError 也留在本层，
// 由 files_service.go 的 toFilesHTTPError 映射为领域的 HTTPError。

// download 是 GET /download 的薄适配（实体：files.Service.Download）。
func (h *Handlers) download(w http.ResponseWriter, r *http.Request) {
	h.fileService().Download(w, r)
}

// stat 是 HEAD /api/files/stat 的薄适配（实体：files.Service.Stat）。
func (h *Handlers) stat(w http.ResponseWriter, r *http.Request) {
	h.fileService().Stat(w, r)
}
