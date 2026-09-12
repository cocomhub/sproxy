// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// files_service.go 是 `pkg/files` 文件服务域的**装配适配**：把 Handlers 持有的装配项
// （配置、日志器、租户/配额/校验和/分块存储的懒建缓存、锁池、容量账本、卷路由与读定位、
// 版本策略、审计、计量）适配为 pkg/files 的**能力接口**（Option 构造），并把路由注册引用的
// 处理器转为一行的薄适配（本文件放分块族；只读面在 list_handler.go，下载/stat 在
// download_handler.go；写面在 upload_handler.go / rename_handler.go / delete_handler.go）。
//
// 装配形态：`filesRuntime`（本文件）实现领域声明的全部能力接口，装配点只需把它传给
// `files.New` + 少量 Option（日志器取用函数、计量条件注入）。nil 语义（未装配卷集合/容量）
// 在适配器内部判 nil → 返回 nil 接口，不再需要逐字段 typed-nil 守卫。
package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// filesStorageManager 把 *capacity.StorageManager 适配为 files.StorageManager。
// 子包不得见 capacity.StorageCategory，故类别由本适配器固定为 CategoryChunked。
type filesStorageManager struct{ m *capacity.StorageManager }

func (a filesStorageManager) TryReserveChunked(n int64) error {
	return a.m.TryReserve(n, capacity.CategoryChunked)
}
func (a filesStorageManager) ReleaseChunked(n int64) { a.m.Release(n, capacity.CategoryChunked) }
func (a filesStorageManager) Usage() int64           { return a.m.Usage() }
func (a filesStorageManager) MaxBytes() int64        { return a.m.MaxBytes() }

// filesRuntime 把 *Handlers 的装配能力适配为 pkg/files 的能力接口（Option 构造用）。
//
// **一个类型实现多个接口**：能力接口按消费方命名且方法集不相交（TenantResolver.TenantFor、
// VolumeRouter.Volumes/Tenant/Locate/Route、QuotaScopes.ScopeFor、
// ChecksumLedgers.ChecksumStoreFor、ChunkedUploads.UploadStoreFor/Capacity、
// Versioning.Enabled/MaxVersions、FileLocks.TryMark/Acquire、Auditor.Record、
// DownloadPaths.Resolve），故单个适配类型即可——装配层不再需要逐字段方法值。
//
// **nil 语义由适配器内部表达**：Volumes()/Capacity() 显式判 h.volSet/h.storageMgr 是否为
// nil 并返回 nil 接口——避免「nil 具体指针装入接口成为非 nil 接口」使领域的「未装配」判断失效。
type filesRuntime struct{ h *Handlers }

func (r filesRuntime) TenantFor(owner string) *storage.Tenant { return r.h.tenantFor(owner) }

func (r filesRuntime) Actor(req *http.Request) string { return ownerFromRequest(req) }

func (r filesRuntime) Volumes() files.VolumeSet {
	if r.h.volSet == nil {
		return nil
	}
	return r.h.volSet
}

func (r filesRuntime) Tenant(volName, owner string) *storage.Tenant {
	return r.h.volumeTenant(volName, owner)
}

func (r filesRuntime) Locate(owner, rel string) (files.FileLocation, bool) {
	return r.h.locateOwnerFileForFiles(owner, rel)
}

func (r filesRuntime) Route(owner, rel, explicitVol string, size int64, forceHomeVol string) (files.UploadRoute, error) {
	return r.h.routeUploadForFiles(owner, rel, explicitVol, size, forceHomeVol)
}

func (r filesRuntime) ScopeFor(owner, rel string) *quota.Scope { return r.h.quotaScopeFor(owner, rel) }

func (r filesRuntime) ChecksumStoreFor(owner string) *checksum.ChecksumStore {
	return r.h.checksumStoreFor(owner)
}

func (r filesRuntime) UploadStoreFor(owner string) *files.UploadStore {
	return r.h.uploadStoreFor(owner)
}

func (r filesRuntime) Capacity() files.StorageManager {
	if r.h.storageMgr == nil {
		return nil
	}
	return filesStorageManager{r.h.storageMgr}
}

func (r filesRuntime) Enabled() bool { return r.h.cfgPtr.Load().Versioning.Enabled }

func (r filesRuntime) MaxVersions() int { return r.h.cfgPtr.Load().Versioning.MaxVersions }

func (r filesRuntime) TryMark(owner, rel, value string) (func(), bool) {
	return r.h.tryMarkUploadingFile(owner, rel, value)
}

func (r filesRuntime) Acquire(owner, rel string) (func(), bool) {
	return r.h.acquireFileLock(owner, rel)
}

func (r filesRuntime) Resolve(req *http.Request) (files.DownloadPath, error) {
	return r.h.resolveDownloadPathForFiles(req)
}

func (r filesRuntime) Record(ctx context.Context, action, object, result, detail string) {
	r.h.RecordAudit(ctx, AuditEvent{
		Action: action, ObjectType: "file", Object: object, Result: result, Detail: detail,
	})
}

// resolveDownloadPathForFiles 把 resolveDownloadPath 的结果适配为子包的值类型，
// 并把 *downloadPathError 映射为子包的 HTTPError（子包不见该错误类型）。
func (h *Handlers) resolveDownloadPathForFiles(r *http.Request) (files.DownloadPath, error) {
	dp, err := h.resolveDownloadPath(r)
	if err != nil {
		return files.DownloadPath{}, toFilesHTTPError(err)
	}
	return files.DownloadPath{Filename: dp.filename, Tenant: dp.tnt, Rel: dp.rel}, nil
}

// locateOwnerFileForFiles 把 locateOwnerFile 的结果适配为子包的值类型。
func (h *Handlers) locateOwnerFileForFiles(owner, rel string) (files.FileLocation, bool) {
	loc, found := h.locateOwnerFile(owner, rel)
	if !found || loc == nil {
		return files.FileLocation{}, false
	}
	return files.FileLocation{VolumeName: loc.volumeName, Tenant: loc.tenant}, true
}

// routeUploadForFiles 把 routeUpload 的结果适配为子包的值类型，
// 并把 *routeError 映射为子包的 HTTPError（保持状态码与文案逐字不变）。
func (h *Handlers) routeUploadForFiles(owner, rel, explicitVol string, size int64, forceHomeVol string) (files.UploadRoute, error) {
	route, err := h.routeUpload(owner, rel, explicitVol, size, forceHomeVol)
	if err != nil {
		return files.UploadRoute{}, toFilesHTTPError(err)
	}
	return files.UploadRoute{
		VolumeName: route.volumeName,
		Tenant:     route.tenant,
		Scope:      route.scope,
		ScopeRes:   route.scopeRes,
		Pool:       route.pool,
		PoolRes:    route.poolRes,
		Release:    route.release,
	}, nil
}

// toFilesHTTPError 把带 HTTP 状态码的 pkg/server 错误（下载路径解析错误 / 卷路由错误）
// 映射为子包的 HTTPError；非该形态返回原错误（子包按各调用点的兜底状态码处理）。
func toFilesHTTPError(err error) error {
	var de *downloadPathError
	if errors.As(err, &de) {
		return &files.HTTPError{Status: de.status, Message: de.message}
	}
	var re *routeError
	if errors.As(err, &re) {
		return &files.HTTPError{Status: re.status, Message: re.msg}
	}
	return err
}

// ---- 路由注册引用的薄适配（路由 pattern 与处理器名逐字不变） ----

func (h *Handlers) uploadInit(w http.ResponseWriter, r *http.Request) {
	h.fileService().UploadInit(w, r)
}

func (h *Handlers) uploadChunk(w http.ResponseWriter, r *http.Request) {
	h.fileService().UploadChunk(w, r)
}

func (h *Handlers) uploadStatus(w http.ResponseWriter, r *http.Request) {
	h.fileService().UploadStatus(w, r)
}

func (h *Handlers) uploadSessions(w http.ResponseWriter, r *http.Request) {
	h.fileService().UploadSessions(w, r)
}

func (h *Handlers) uploadComplete(w http.ResponseWriter, r *http.Request) {
	h.fileService().UploadComplete(w, r)
}

func (h *Handlers) downloadChunk(w http.ResponseWriter, r *http.Request) {
	h.fileService().DownloadChunk(w, r)
}

// 编译期断言：*Metrics 满足领域的计量窄接口（零适配代码，无需包装）。
var _ files.Metrics = (*Metrics)(nil)
