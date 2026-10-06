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
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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
// VolumeRouter.Volumes/Tenant/Locate/Route、QuotaScopeProvider.ScopeFor、
// ChecksumLedgerProvider.ChecksumStoreFor、ChunkedUploads.UploadStoreFor/Capacity、
// Versioning.Enabled/MaxVersions、FileLocks.TryMark/Acquire、Auditor.Record、
// DownloadPathResolver.Resolve），故单个适配类型即可——装配层不再需要逐字段方法值。
//
// **nil 语义由适配器内部表达**：Volumes()/Capacity() 显式判 h.volSet/h.storageMgr 是否为
// nil 并返回 nil 接口——避免「nil 具体指针装入接口成为非 nil 接口」使领域的「未装配」判断失效。
type filesRuntime struct{ h *Handlers }

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
	if cs := r.h.checksumStoreFor(owner); cs != nil {
		if sc, ok := cs.(*checksum.ChecksumStore); ok {
			return sc
		}
	}
	return nil
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

func (r filesRuntime) Retention() time.Duration { return r.h.cfgPtr.Load().Versioning.Retention }

func (r filesRuntime) TryMark(owner, rel, value string) (func(), bool) {
	return r.h.tryMarkUploadingFile(owner, rel, value)
}

func (r filesRuntime) Acquire(owner, rel string) (func(), bool) {
	return r.h.acquireFileLock(owner, rel)
}

func (r filesRuntime) Resolve(req *http.Request) (files.DownloadPath, error) {
	return r.h.resolveDownloadPathForFiles(req)
}

// BucketFor 实现 files.BandwidthLimiter：按 owner 懒建 per-owner 带宽令牌桶
// （带宽限速开关 cfg.RateLimit.Bandwidth 关闭时恒 nil = 不限速，零回归）。
// 桶按 (owner) 缓存，互不影响（per-owner 隔离）；owner 桶容量/速率取配置
// rate_limit.bandwidth.per_owner_bps（bytes/sec）+ burst（默认 = 1 秒配额）。
func (r filesRuntime) BucketFor(owner string) *files.TokenBucket {
	cfg := r.h.cfgPtr.Load()
	if !cfg.RateLimit.Bandwidth.Enabled || cfg.RateLimit.Bandwidth.PerOwnerBPS <= 0 {
		return nil
	}
	return r.h.bwBucketFor(owner, cfg.RateLimit.Bandwidth.PerOwnerBPS, cfg.RateLimit.Bandwidth.Burst)
}

func (r filesRuntime) Record(ctx context.Context, action, object, result, detail string) {
	r.h.RecordAudit(ctx, AuditEvent{
		Action: action, ObjectType: "file", Object: object, Result: result, Detail: detail,
	})
}

// DedupEnabled 返回内容寻址去重是否启用（dedup.enabled 配置，默认关零回归）。
func (r filesRuntime) DedupEnabled() bool {
	return r.h.cfgPtr.Load().Dedup.Enabled
}

// DedupStoreFor 返回 owner 的 per-tenant 去重台账（未启用 → nil）。
// 台账路径：<tenant meta>/dedup.json（懒建缓存，重启扫描恢复）。
func (r filesRuntime) DedupStoreFor(owner string) *files.DedupStore {
	return r.h.dedupStoreFor(owner)
}

// resolveDownloadPathForFiles 把 resolveDownloadPath 的结果适配为子包的值类型，
// 并把 *downloadPathError 映射为子包的 HTTPError（子包不见该错误类型）。
func (h *Handlers) resolveDownloadPathForFiles(r *http.Request) (files.DownloadPath, error) {
	dp, err := h.resolveDownloadPath(r)
	if err != nil {
		return files.DownloadPath{}, toFilesHTTPError(err)
	}
	return files.DownloadPath{Filename: dp.filename, VolumeName: dp.volName, Tenant: dp.tnt, Rel: dp.rel, RedirectURL: dp.redirectURL, Source: dp.source}, nil
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
		Sink:       route.sink,
		Scope:      route.scope,
		ScopeRes:   route.scopeRes,
		Pool:       route.pool,
		PoolRes:    route.poolRes,
		Release:    route.release,
	}, nil
}

// downloadPathForRemote 把**远程只读面**的 (owner, volName, remotePath) 解析为领域层的
// DownloadPath（D-2 后远程面直调域 API，故需要一个显式入参的解析器）。
//
// 为什么不能复用 resolveDownloadPath：后者是 **request 形状**的（读 ?filename/?kind/?volume，
// 并从 ctx 取 actor），而远程面的入参是「授权得出的 owner + 已授权卷 + 路径」——形状不同，
// 硬塞只会把「伪造 actor」这类 hack 固化下来。
//
// 语义（与既有 resolveDownloadPath 的普通文件分支对齐）：
//   - pathguard 校验路径（穿越/内部前缀一律拒 → 400）；
//   - 卷租户由 volumeTenant 解析（卷已由远程面授权，此处不再做 ACL 判定）；
//   - UserRel 映射到 user 桶内相对路径（功能桶不可达 → 400）。
func (h *Handlers) downloadPathForRemote(ctx context.Context, owner, volName, remotePath string) (files.DownloadPath, error) {
	rel0, err := pathguard.ValidateFilePath(remotePath)
	if err != nil {
		msg := errMsgInvalidFilename
		if remotePath == "" {
			msg = errMsgEmptyFilename
		}
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusBadRequest, Message: msg}
	}
	tnt := h.volumeTenant(volName, owner)
	// **外部卷远程读（2026-10-05 用户裁定：本期含 secretdata 持有者）**：持有节点
	// 数据可能是 secretdata 等外部卷（无 *storage.Tenant）——经装配层服务端读取源
	// （externalDownloadSource）构造 DownloadPath.Source，OpenPath/StatPath 的 Source
	// 分支解密转发（#735 已实现）。
	if tnt == nil || tnt.Root() == nil {
		return h.externalRemotePath(ctx, owner, volName, rel0, remotePath)
	}
	rel, ok := tnt.UserRel(rel0)
	if !ok {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	return files.DownloadPath{Filename: remotePath, Tenant: tnt, Rel: rel}, nil
}

// externalRemotePath 是 downloadPathForRemote 的外部卷分支（持有节点数据是 secretdata
// 等外部卷时）：经装配层服务端读取源构造 DownloadPath.Source（解密转发）。
func (h *Handlers) externalRemotePath(ctx context.Context, owner, volName, rel0, remotePath string) (files.DownloadPath, error) {
	fsys, ok := h.externalFSFor(volName)
	if !ok {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	v, ok := h.volSet.ByName(volName)
	if !ok {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	ownerKey, kerr := v.ResolveUserPath(owner, rel0)
	if kerr != nil {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	e, serr := fsys.Stat(ctx, ownerKey)
	if serr != nil || e == nil || e.IsDir {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	return files.DownloadPath{
		Filename: remotePath, VolumeName: volName,
		Rel: ownerKey, Source: newExternalSource(fsys, ownerKey, e),
	}, nil
}

// externalFSFor 返回外部卷的 FS 视图（未装配/无 FS → (nil,false)）。
func (h *Handlers) externalFSFor(volName string) (syncpkg.FS, bool) {
	if h.volSet == nil {
		return nil, false
	}
	be := h.volSet.External(volName)
	if be == nil {
		return nil, false
	}
	fsys := be.FS()
	return fsys, fsys != nil
}

// toFilesHTTPError 把带 HTTP 状态码的 pkg/server 错误（下载路径解析错误 / 卷路由错误）
// 映射为子包的 HTTPError；非该形态返回原错误（子包按各调用点的兜底状态码处理）。
func toFilesHTTPError(err error) error {
	if de, ok := errors.AsType[*downloadPathError](err); ok {
		return &files.HTTPError{Status: de.status, Message: de.message}
	}
	if re, ok := errors.AsType[*routeError](err); ok {
		return &files.HTTPError{Status: re.status, Message: re.msg}
	}
	return err
}

// filesMetaPolicy 实现 files.FileMetaPolicy（可信卷 meta 能力装配）：本地卷上传
// 到达即建配套 .meta（隐藏、占配额）。由 trusted_volume.disable 开关控制
// （缺省 false = 启用）。WriteMeta 从已落盘文件计算 FileMeta（总/分块 sha256+md5）
// 并原子写 `.meta` sidecar（经 root，占配额计入底层账本）。
type filesMetaPolicy struct{ h *Handlers }

var _ files.FileMetaPolicy = filesMetaPolicy{}

// Enabled 报告可信卷 meta 是否启用（trusted_volume.disable 缺省 false）。
func (p filesMetaPolicy) Enabled() bool { return !p.h.trustedDisabled() }

// WriteMeta 计算并写入配套 .meta（本地卷上传到达即建；失败返回错误由调用方 Warn 兜底，
// 读路径 Stat 直算不依赖 meta 存在）。
func (p filesMetaPolicy) WriteMeta(ctx context.Context, root *storage.Root, rel string) error {
	// 计算 FileMeta：从已落盘文件（root 相对 rel）读取计算总 sha256+md5 + 分块。
	fm, err := p.computeMeta(ctx, root, rel)
	if err != nil {
		return err
	}
	data, err := meta.Marshal(fm)
	if err != nil {
		return err
	}
	// 原子写 `.meta` sidecar（同目录隐藏文件；占配额——经 root 写，底层账本计入）。
	mrel := meta.MetaPath(rel)
	dir := path.Dir(rel)
	if dir != "." && dir != "" {
		if mkErr := root.MkdirAll(dir, 0o755); mkErr != nil {
			return mkErr
		}
	}
	// 复用存储层原子写（root 相对 + fsync + rename）：与主文件同语义。
	tmpRel := mrel + ".tmp"
	_ = root.Remove(tmpRel)
	f, ferr := root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if ferr != nil {
		return ferr
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		_ = root.Remove(tmpRel)
		return werr
	}
	if cerr != nil {
		_ = root.Remove(tmpRel)
		return cerr
	}
	if rerr := root.Rename(tmpRel, mrel); rerr != nil {
		_ = root.Remove(tmpRel)
		return rerr
	}
	return nil
}

// computeMeta 从 root 相对 rel 的文件计算完整 FileMeta（流式读一遍，双算法 + 分块）。
func (p filesMetaPolicy) computeMeta(ctx context.Context, root *storage.Root, rel string) (*meta.FileMeta, error) {
	rc, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("可信卷 meta 打开文件 %s: %w", rel, err)
	}
	defer rc.Close()
	fi, serr := rc.Stat()
	if serr != nil {
		return nil, fmt.Errorf("可信卷 meta stat %s: %w", rel, serr)
	}
	c, cerr := meta.NewCalculator(fi.Size(), 0)
	if cerr != nil {
		return nil, cerr
	}
	if _, cerr := c.ReadFrom(rc); cerr != nil {
		return nil, fmt.Errorf("可信卷 meta 计算 %s: %w", rel, cerr)
	}
	fm := c.Finish()
	fm.Name = path.Base(strings.ReplaceAll(rel, "\\", "/"))
	return fm, nil
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
