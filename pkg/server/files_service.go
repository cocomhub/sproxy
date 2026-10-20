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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// filesStorageManager 把 *capacity.StorageManager 适配为 files.StorageManager。
// 子包不得见 capacity.StorageCategory，故类别由本适配器固定为 CategoryChunked。
type filesStorageManager struct{ m *capacity.StorageManager }

// WaitReserveChunked 预留分块上传的全局容量：**收敛到容量层等待入口**
// （capacity.StorageManager.WaitReserve）⇒ 容量不足时**排队重试**（释放唤醒 / ctx 取消 /
// 超时），而不是立即 507 —— 与配额层同款语义（用户裁定「严格上界预占 + 不够时继续队列重试等待」）。
// 保留本方法（P5 回退路径：quota 未装配时的全局容量回退）——**能力不丢失**。
func (a filesStorageManager) WaitReserveChunked(ctx context.Context, n int64) error {
	return a.m.WaitReserve(ctx, n, capacity.CategoryChunked, capacity.DefaultWaitTimeout)
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
	return filesVolumeSet{Set: r.h.volSet}
}

// filesVolumeSet 把 *registry.Set 适配为 files.VolumeSet + files.ExternalVolumeSource：
// 外部卷目录浏览能力经 registry.Set.External(name).FS()（sync.FS）适配为领域接口。
// 嵌入 *registry.Set 继承 VolumeSet 方法（Default/All/ByName/Root/Pool），外部能力新增。
type filesVolumeSet struct {
	*registry.Set
}

// CategoryOf 实现 files.VolumeTypeClassifier：把卷**类型 → schema category** 分类转发给
// registry.CategoryOf——「哪些类型是 wrapper」的唯一事实源（含 wrapper 处理层注册表的动态
// 投影，见 enclosure.RegisterWrapper → registry.MarkWrapperType）。领域包不得 import
// registry（门禁 R2），故经本适配器注入（装配处 WithVolumeTypeClassifier）。
//
// 无状态转发：不读嵌入的 *registry.Set（nil 也安全），故装配处传 `filesVolumeSet{Set: h.volSet}`
// 与其它适配器同形。
func (a filesVolumeSet) CategoryOf(typ string) string { return registry.CategoryOf(typ) }

// ExternalVolume 实现 files.ExternalVolumeSource：按卷名返回外部卷目录浏览能力。
// 未知/非外部卷（本地卷）→ nil（List 的 `?volume=` 回落既有本地聚合，零回归）。
func (a filesVolumeSet) ExternalVolume(name string) files.ExternalVolume {
	be := a.External(name)
	if be == nil {
		return nil
	}
	fsys := be.FS()
	if fsys == nil {
		return nil
	}
	return filesExternalVolume{fs: fsys}
}

// ExternalDeleter 实现 files.ExternalVolumeDeleter（C2，2026-10-06）：按卷名返回外部卷删除
// 能力。未知/非外部卷（本地卷）/FS 不可用 → nil（delete 走既有本地卷路径，零回归）。
// key 语义与上传 externalUploadSink 一致：ResolveUserPath 统一计算（owner 前缀/键空间适配）。
func (a filesVolumeSet) ExternalDeleter(name string) files.ExternalDeleter {
	be := a.External(name)
	if be == nil {
		return nil
	}
	fsys := be.FS()
	if fsys == nil {
		return nil
	}
	v, ok := a.ByName(name)
	if !ok {
		return nil
	}
	return filesExternalDeleter{fs: fsys, v: v}
}

// filesExternalDeleter 把 sync.FS + 卷描述适配为 files.ExternalDeleter。
type filesExternalDeleter struct {
	fs syncpkg.FS
	v  volume.Volume
}

// ownerKey 把域侧 rel（user/<name>）映射为外部卷键（与 externalUploadSink 同一算法：
// 桶目录自身 "user" → 桶内空路径，防 "user/user" 残留）。基于 locator：ResolveUserLocation
// 解析 + FSPath 拼接（键拼接只在 FSPath，调用方不自行改路径）。
//
// 键空间**恒 owner 维度**（`<owner>/user/<rel>`，含嵌套封装卷）：所有外部卷走同一
// locator，调用方（上传/删除/列举/转存/镜像/du）不需要区分卷类型。
func (a filesExternalDeleter) ownerKey(owner, rel string) (string, error) {
	stripped := rel
	switch {
	case rel == "user":
		stripped = ""
	case strings.HasPrefix(rel, "user/"):
		stripped = strings.TrimPrefix(rel, "user/")
	}
	loc, err := a.v.ResolveUserLocation(owner, stripped)
	if err != nil {
		return "", err
	}
	return loc.FSPath(), nil
}

// StatSize 实现 files.ExternalDeleter。
func (a filesExternalDeleter) StatSize(ctx context.Context, _, owner, rel string) (int64, bool, error) {
	key, err := a.ownerKey(owner, rel)
	if err != nil {
		return 0, false, err
	}
	e, serr := a.fs.Stat(ctx, key)
	if serr != nil {
		return 0, false, serr
	}
	if e == nil {
		return 0, false, nil
	}
	if e.IsDir {
		return 0, true, nil
	}
	return e.Size, true, nil
}

// FileChecksum 实现 files.ExternalFileChecksum（评审 CRIT-1）：从外部卷 FS 流式读取明文
// 内容计算 SHA-256（hex）。外部卷（secretdata 等）的 FS 读即解密密文 → 明文指纹，与上传
// 时的客户端 checksum 可比。不可用/目录 → 返回错误（调用方 500 保留，不静默删）。
func (a filesExternalDeleter) FileChecksum(ctx context.Context, _, owner, rel string) (string, error) {
	key, err := a.ownerKey(owner, rel)
	if err != nil {
		return "", err
	}
	rc, oerr := a.fs.OpenRead(ctx, key)
	if oerr != nil {
		return "", oerr
	}
	defer rc.Close()
	h := sha256.New()
	if _, cerr := io.Copy(h, rc); cerr != nil {
		return "", cerr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Delete 实现 files.ExternalDeleter。
func (a filesExternalDeleter) Delete(ctx context.Context, _, owner, rel string) error {
	key, err := a.ownerKey(owner, rel)
	if err != nil {
		return err
	}
	return a.fs.Delete(ctx, key)
}

// filesExternalVolume 把 sync.FS 的 ListDir 适配为 files.ExternalVolume
// （[]syncpkg.Entry → []files.ExternalEntry，只保留 Name/IsDir/Size 明文视图）。
type filesExternalVolume struct{ fs syncpkg.FS }

func (a filesExternalVolume) ListDir(ctx context.Context, rel string) ([]files.ExternalEntry, error) {
	entries, err := a.fs.ListDir(ctx, rel)
	if err != nil {
		return nil, err
	}
	out := make([]files.ExternalEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, files.ExternalEntry{Name: e.Name, IsDir: e.IsDir, Size: e.Size})
	}
	return out, nil
}

func (r filesRuntime) Tenant(volName, owner string) *storage.Tenant {
	return r.h.volumeTenant(volName, owner)
}

func (r filesRuntime) Locate(owner, rel string) (files.FileLocation, bool) {
	return r.h.locateOwnerFileForFiles(owner, rel)
}

func (r filesRuntime) Route(ctx context.Context, owner, rel, explicitVol string, size int64, forceHomeVol string) (files.UploadRoute, error) {
	return r.h.routeUploadForFiles(ctx, owner, rel, explicitVol, size, forceHomeVol)
}

func (r filesRuntime) RouteTarget(ctx context.Context, owner, rel, volName string, size int64) (files.UploadRoute, error) {
	route, err := r.h.routeUploadForTarget(ctx, owner, rel, volName, size)
	if err != nil {
		return files.UploadRoute{}, err
	}
	return route, nil
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
	return files.DownloadPath{Filename: dp.filename, VolumeName: dp.volName, Tenant: dp.tnt, Rel: dp.rel, RedirectURL: dp.redirectURL, Source: dp.source, Ciphertext: dp.ciphertext}, nil
}

// locateOwnerFileForFiles 把 locateOwnerFile 的结果适配为子包的值类型。
func (h *Handlers) locateOwnerFileForFiles(owner, rel string) (files.FileLocation, bool) {
	loc, found := h.locateOwnerFile(owner, rel)
	if !found || loc == nil {
		return files.FileLocation{}, false
	}
	return files.FileLocation{VolumeName: loc.volumeName, Tenant: loc.tenant}, true
}

// routeUploadForTarget 为统一写内核（WriteTarget 显式卷：S3 单对象 PUT / DAV / 版本恢复 /
// presign 直传登记）定卷：ACL 校验 + 写保护 + 单候选双预留，**不做唯一性查重**（覆盖写语义
// ——这些写面允许覆盖同 rel）。
func (h *Handlers) routeUploadForTarget(ctx context.Context, owner, rel, volName string, size int64) (files.UploadRoute, error) {
	if h.volSet == nil {
		return h.routeUploadForFiles(ctx, owner, rel, volName, size, "")
	}
	owner = normalizeOwner(owner)
	view := volume.AllowedVolumes(h.volSet.All(), owner)
	if len(view) == 0 {
		return files.UploadRoute{}, newRouteError(routeErrNotAllowed, http.StatusForbidden, msgVolumeNotAllowed, nil)
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return files.UploadRoute{}, newRouteError(routeErrNotAllowed, http.StatusForbidden, msgVolumeNotAllowed, nil)
	}
	route, err := h.reserveVolume(ctx, owner, rel, v.Name, size)
	if err != nil {
		// 预留失败必须是契约错误（*files.HTTPError）：调用方（s3WriteError / DAV / presign）
		// 只认该类型，裸 *routeError 会退化成 500（配额满应 507、越权应 403）。
		return files.UploadRoute{}, toFilesHTTPError(err)
	}
	release := route.release
	return files.UploadRoute{
		VolumeName: route.volumeName, Tenant: route.tenant, Sink: route.sink,
		Scope: route.scope, ScopeRes: route.scopeRes, Pool: route.pool, PoolRes: route.poolRes,
		PhysSize: files.TenantPhysSizer(route.tenant),
		Release:  release,
	}, nil
}

// routeUploadForFiles 把 routeUpload 的结果适配为子包的值类型，
// 并把 *routeError 映射为子包的 HTTPError（保持状态码与文案逐字不变）。
func (h *Handlers) routeUploadForFiles(ctx context.Context, owner, rel, explicitVol string, size int64, forceHomeVol string) (files.UploadRoute, error) {
	route, err := h.routeUpload(ctx, owner, rel, explicitVol, size, forceHomeVol)
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
		PhysSize:   files.TenantPhysSizer(route.tenant),
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
	key, ok := externalVolumeKey(v, owner, rel0)
	if !ok {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	// 基于 locator：FS 键仅在此经 FSPath 拼接（唯一拼接点）。
	ownerKey := key
	e, serr := fsys.Stat(ctx, ownerKey)
	if serr != nil || e == nil || e.IsDir {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	return files.DownloadPath{
		Filename: remotePath, VolumeName: volName,
		Rel: ownerKey, Source: newExternalSource(fsys, ownerKey, e, h.verifySkipped()),
	}, nil
}

// externalVolumeOps 实现 files.ExternalVolumeOps（评审 F1：外部卷对象的**硬删**入口）。
type externalVolumeOps struct{ h *Handlers }

// ExternalDelete 经装配层**同一可信链**删除外部卷对象：
// `Guard(Wrap(CapacityFS(raw)))` 的 Delete 完成「删主文件 + 联动删 sidecar + 释放卷级计数」。
//
// 不经回收站、不做 quarantine（远端后端无原子 rename）；`expectedChecksum` 非空时与 sidecar
// **记录**的整文件 SHA-256 交叉比对（不重算——远端整文件回读不可接受）。
func (o externalVolumeOps) ExternalDelete(ctx context.Context, owner, volName, remotePath, expectedChecksum string) (bool, error) {
	h := o.h
	if h.volSet == nil {
		return false, nil
	}
	fsys, ok := h.externalFSFor(volName) // Guard(Wrap(be.FS()))；be.FS() 已含 CapacityFS
	if !ok {
		return false, nil
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return false, nil // 未授权 → 不命中（404，不泄卷存在性）
	}
	key, ok := externalVolumeKey(v, owner, remotePath)
	if !ok {
		return false, nil
	}
	e, serr := fsys.Stat(ctx, key)
	if serr != nil || e == nil || e.IsDir {
		return false, nil // 不存在/不可 stat → 未命中
	}
	if cerr := crossCheckExternalChecksum(ctx, fsys, key, expectedChecksum); cerr != nil {
		return false, cerr
	}
	if derr := fsys.Delete(ctx, key); derr != nil {
		if errors.Is(derr, storage.ErrSealed) {
			return false, &files.HTTPError{Status: http.StatusConflict, Message: "该路径由封装卷管理，禁止直接删除"}
		}
		h.logger.ErrorContext(ctx, "外部卷删除失败", "volume", volName, "path", remotePath, "error", derr.Error())
		return false, &files.HTTPError{Status: http.StatusInternalServerError, Message: "删除失败"}
	}
	// 配额释放（owner 全局账本 + 委托卷容量池）**不在本层**：域侧 `deleteExternalVolume` 在
	// `ExternalDelete` 成功返回后统一调 `releaseExternalDeleteQuota`（删除前实测记账字节，
	// 与写入侧 `settleSize` 同口径）——这是委派池的**唯一**域层释放点。此前本层另有一次
	// `releaseWrapperPoolAfterDelete`，与域侧对同一委托子 Scope 重复归还（F3：底座池少计 ⇒
	// 过度准入），已移除：一次删除只允许一处释放。
	return true, nil
}

// externalVolumeKey 计算外部卷对象操作（Delete/Stat/List/Rename 等）的 FS 键：
// **全部外部卷统一经 `ResolveUserLocation` + `FSPath()` 解析**（恒 `<owner>/user/<rel>`，
// owner 维度隔离）。调用方（上传 / 删除 / 列举 / 转存 / 跨卷 move / 镜像 / du）共用同一
// 映射口径，不存在卷类型分支。
//
// 返回 (key, false) = 路径非法（ResolveUserLocation 失败），调用方按未命中处理。
func externalVolumeKey(v volume.Volume, owner, remotePath string) (string, bool) {
	loc, kerr := v.ResolveUserLocation(owner, remotePath)
	if kerr != nil {
		return "", false
	}
	return loc.FSPath(), true
}

// ExternalList 实现 files.ExternalVolumeOps：经**同一可信链**（`Guard(Wrap(be.FS()))`）
// 列举外部卷目录（用户可见相对路径经 `ResolveUserLocation` 翻译为卷内键——共享卷自动加
// owner 前缀隔离）。语义：非外部卷/未授权/未装配 → (nil,false,nil)（调用方零回归）；
// 目录不存在 → 空列表；远端/IO 失败 → 500。
func (o externalVolumeOps) ExternalList(ctx context.Context, owner, volName, subdir string) ([]files.FileInfo, bool, error) {
	h := o.h
	if h.volSet == nil {
		return nil, false, nil
	}
	fsys, ok := h.externalFSFor(volName)
	if !ok {
		return nil, false, nil
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return nil, false, nil // 未授权/未知 → 不命中（404，不泄卷存在性）
	}
	key, ok := externalVolumeKey(v, owner, subdir)
	if !ok {
		return nil, true, &files.HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	entries, lerr := fsys.ListDir(ctx, key)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			return nil, true, nil // 目录不存在 = 空列表
		}
		h.logger.Warn("外部卷列举失败", "volume", volName, "subdir", subdir, "error", lerr.Error())
		return nil, true, &files.HTTPError{Status: http.StatusInternalServerError, Message: "列举失败"}
	}
	out := make([]files.FileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, files.FileInfo{
			Name: e.Name, IsDir: e.IsDir, Size: e.Size, ModTime: e.MTime, Volume: volName, // ModTime = UnixNano
		})
	}
	return out, true, nil
}

// ExternalStat 实现 files.ExternalVolumeOps：外部卷对象元信息（明文 checksum 经 meta.Provider）。
func (o externalVolumeOps) ExternalStat(ctx context.Context, owner, volName, remotePath string) (int64, string, error) {
	h := o.h
	if h.volSet == nil {
		return -1, "", nil
	}
	fsys, ok := h.externalFSFor(volName)
	if !ok {
		return -1, "", nil
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return -1, "", nil
	}
	key, ok := externalVolumeKey(v, owner, remotePath)
	if !ok {
		return -1, "", &files.HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	e, serr := fsys.Stat(ctx, key)
	if serr != nil || e == nil || e.IsDir {
		return -1, "", nil
	}
	cs := ""
	if pv, ok := fsys.(meta.Provider); ok {
		if fm, ferr := pv.FileMeta(ctx, key); ferr == nil && fm != nil {
			cs = fm.TotalSHA256
		}
	}
	return e.Size, cs, nil
}

// ExternalCopy 实现 files.ExternalVolumeOps：同卷内复制。卷实现 blob 直拷能力（`CopyFile`，
// 见 pkg/volume/secretdata）⇒ 底层 blob 直拷 + 新 meta（**不重新加解密**）；否则
// Supported=false（调用方回落统一写内核）。
func (o externalVolumeOps) ExternalCopy(ctx context.Context, owner, volName, fromRemote, fromRel, toRemote, toRel string, force bool) (files.ExternalCopyResult, error) {
	h := o.h
	srcKey, dstKey, fsys, ok := h.externalCopyKeys(owner, volName, fromRemote, toRemote)
	if !ok {
		return files.ExternalCopyResult{}, nil // 未装配/非外部卷/未授权/不支持 → 回落写内核
	}
	if err := h.prepareExternalCopyDest(ctx, fsys, dstKey, force); err != nil {
		return files.ExternalCopyResult{Supported: true}, err
	}
	res := files.ExternalCopyResult{Supported: true}
	if pv, ok := fsys.(meta.Provider); ok {
		if fm, ferr := pv.FileMeta(ctx, srcKey); ferr == nil && fm != nil {
			res.Checksum, res.Size = fm.TotalSHA256, fm.Size
		}
	}
	if cerr := h.copyWithinVolumeFS(ctx, volName, fromRemote, toRemote, fsys, srcKey, dstKey); cerr != nil {
		return res, cerr
	}
	_ = fromRel
	_ = toRel
	return res, nil
}

// externalCopyKeys 解析同卷复制的源/目标键并做能力断言。ok=false 表示「非本接缝可处理」
// （未装配 / 非外部卷 / 未授权 / 该卷类型不支持 blob 直拷）⇒ 调用方回落写内核。
func (h *Handlers) externalCopyKeys(owner, volName, fromRemote, toRemote string) (string, string, syncpkg.FS, bool) {
	if h.volSet == nil {
		return "", "", nil, false
	}
	be := h.volSet.External(volName)
	if be == nil {
		return "", "", nil, false
	}
	raw := be.FS()
	if raw == nil {
		return "", "", nil, false
	}
	// 能力断言必须**穿透装饰器**（装配层会包 CapacityFS/trusted 等）——否则「卷支持 blob 直拷」
	// 会被判为不支持，静默回落到写内核（重新加密）。
	if _, ok := syncpkg.Innermost(raw).(interface {
		CopyFile(ctx context.Context, from, to string) error
	}); !ok {
		return "", "", nil, false
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return "", "", nil, false
	}
	srcKey, srcOK := externalVolumeKey(v, owner, fromRemote)
	dstKey, dstOK := externalVolumeKey(v, owner, toRemote)
	if !srcOK || !dstOK {
		return "", "", nil, true // 已裁决（路径非法）→ 由调用方映射 400
	}
	fsys, ok := h.externalFSFor(volName)
	if !ok {
		return "", "", nil, false
	}
	return srcKey, dstKey, fsys, true
}

// prepareExternalCopyDest 目标已存在处理：默认 409（不静默覆盖）；force 时先删（幂等）。
func (h *Handlers) prepareExternalCopyDest(ctx context.Context, fsys syncpkg.FS, dstKey string, force bool) error {
	if !force {
		if e, derr := fsys.Stat(ctx, dstKey); derr == nil && e != nil {
			return &files.HTTPError{Status: http.StatusConflict, Message: "目标路径已存在"}
		}
		return nil
	}
	if derr := fsys.Delete(ctx, dstKey); derr != nil && !os.IsNotExist(derr) {
		return &files.HTTPError{Status: http.StatusInternalServerError, Message: "删除已存在目标失败"}
	}
	return nil
}

// copyWithinVolumeFS 调（最内层）卷的 CopyFile，并把底层错误映射为领域错误。
func (h *Handlers) copyWithinVolumeFS(ctx context.Context, volName, fromRemote, toRemote string, raw syncpkg.FS, srcKey, dstKey string) error {
	copier, ok := syncpkg.Innermost(raw).(interface {
		CopyFile(ctx context.Context, from, to string) error
	})
	if !ok {
		return &files.HTTPError{Status: http.StatusNotImplemented, Message: "该卷类型不支持复制"}
	}
	if cerr := copier.CopyFile(ctx, srcKey, dstKey); cerr != nil {
		if errors.Is(cerr, storage.ErrSealed) {
			return &files.HTTPError{Status: http.StatusConflict, Message: "该路径由封装卷管理，禁止直接复制"}
		}
		h.logger.ErrorContext(ctx, "外部卷复制失败", "volume", volName, "from", fromRemote, "to", toRemote, "error", cerr.Error())
		return &files.HTTPError{Status: http.StatusInternalServerError, Message: "复制失败"}
	}
	return nil
}

// ExternalMkdir 实现 files.ExternalVolumeOps：经**同一可信链**在外部卷内创建目录
// （用户可见相对路径 → 卷内键经 ResolveUserLocation 翻译）。幂等：目录已存在由该卷
// `MakeDir` 语义决定（secretdata 的 ensureContainer 幂等）；封印 → 409。
func (o externalVolumeOps) ExternalMkdir(ctx context.Context, owner, volName, dir string) (bool, error) {
	h := o.h
	if h.volSet == nil {
		return false, nil
	}
	fsys, ok := h.externalFSFor(volName)
	if !ok {
		return false, nil
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return false, nil
	}
	loc, kerr := v.ResolveUserLocation(owner, dir)
	if kerr != nil {
		return true, &files.HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	if merr := fsys.MakeDir(ctx, loc.FSPath()); merr != nil {
		if errors.Is(merr, storage.ErrSealed) {
			return true, &files.HTTPError{Status: http.StatusConflict, Message: "该路径由封装卷管理，禁止直接创建目录"}
		}
		h.logger.ErrorContext(ctx, "外部卷创建目录失败", "volume", volName, "dir", dir, "error", merr.Error())
		return true, &files.HTTPError{Status: http.StatusInternalServerError, Message: "创建目录失败"}
	}
	return true, nil
}

// ExternalRemoveDir 实现 files.ExternalVolumeOps：删除外部卷上的目录（`force=true` 时递归）。
//
// 语义：非外部卷/未授权/未装配 → (false, nil)（调用方零回归）；否则 (true, err)，err 为
// `*files.HTTPError`（400 非法路径/不是目录/未确认、404 不存在、409 封印、500 IO）。
// 递归删先子后父（目录自身最后删）；深度上限防异常后端把键空间挂成环。
func (o externalVolumeOps) ExternalRemoveDir(ctx context.Context, owner, volName, dir string, force bool) (bool, error) {
	h := o.h
	if h.volSet == nil {
		return false, nil
	}
	fsys, ok := h.externalFSFor(volName)
	if !ok {
		return false, nil
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return false, nil
	}
	loc, kerr := v.ResolveUserLocation(owner, dir)
	if kerr != nil {
		return true, &files.HTTPError{Status: http.StatusBadRequest, Message: "无效的目录路径"}
	}
	key := loc.FSPath()
	e, serr := fsys.Stat(ctx, key)
	if serr != nil || e == nil {
		return true, &files.HTTPError{Status: http.StatusNotFound, Message: "目录不存在"}
	}
	if !e.IsDir {
		return true, &files.HTTPError{Status: http.StatusBadRequest, Message: "指定路径不是目录"}
	}
	// 与本地 rmdir 同语义：必须显式 force=true 才删（绝不静默递归删用户内容）。
	if !force {
		return true, &files.HTTPError{Status: http.StatusBadRequest, Message: "请使用 ?force=true 确认删除"}
	}
	if derr := h.deleteExternalTree(ctx, fsys, key, 0); derr != nil {
		return true, derr
	}
	return true, nil
}

// maxExternalTreeDepth 是外部卷目录树递归删除的深度上限（防异常后端 / 键空间环）。
const maxExternalTreeDepth = 64

// deleteExternalTree 递归删除外部卷目录树（先子目录、后文件、最后目录自身）。
// 错误映射与 ExternalDelete 同口径（封印 → 409；其它 IO → 500 + 服务端日志）。
func (h *Handlers) deleteExternalTree(ctx context.Context, fsys syncpkg.FS, key string, depth int) error {
	if depth > maxExternalTreeDepth {
		return &files.HTTPError{Status: http.StatusInternalServerError, Message: "目录层级过深，拒绝递归删除"}
	}
	entries, err := fsys.ListDir(ctx, key)
	if err != nil {
		h.logger.ErrorContext(ctx, "外部卷列目录失败（递归删）", "path", key, "error", err.Error())
		return &files.HTTPError{Status: http.StatusInternalServerError, Message: "读取目录失败"}
	}
	for _, e := range entries {
		child := path.Join(key, e.Name)
		if e.IsDir {
			if derr := h.deleteExternalTree(ctx, fsys, child, depth+1); derr != nil {
				return derr
			}
			continue
		}
		if derr := fsys.Delete(ctx, child); derr != nil {
			return h.mapExternalDeleteErr(ctx, child, derr)
		}
	}
	if derr := fsys.Delete(ctx, key); derr != nil {
		return h.mapExternalDeleteErr(ctx, key, derr)
	}
	// secretdata 等「目录不进文件索引」的卷：Delete 对目录键是 no-op ⇒ 需可选能力显式删容器。
	// 能力断言必须**穿透装饰器**（Guard/Wrap/CapacityFS），且 Delete 已先行 ⇒ 封印（ErrSealed）
	// 与容量联动语义不因走最内层而绕过（封印在 Delete 一步已 fail-closed）。
	if rd, ok := syncpkg.Innermost(fsys).(interface {
		RemoveDir(ctx context.Context, path string) error
	}); ok {
		if derr := rd.RemoveDir(ctx, key); derr != nil {
			return h.mapExternalDeleteErr(ctx, key, derr)
		}
	}
	return nil
}

// mapExternalDeleteErr 把外部卷删除错误映射为领域错误（封印 409；其它 500）。
func (h *Handlers) mapExternalDeleteErr(ctx context.Context, key string, err error) error {
	if errors.Is(err, storage.ErrSealed) {
		return &files.HTTPError{Status: http.StatusConflict, Message: "该路径由封装卷管理，禁止直接删除"}
	}
	h.logger.ErrorContext(ctx, "外部卷删除失败", "path", key, "error", err.Error())
	return &files.HTTPError{Status: http.StatusInternalServerError, Message: "删除失败"}
}

// ExternalRename 实现 files.ExternalVolumeOps：经**同一可信链**在外部卷内重命名
// （用户可见相对路径 → 卷内键经 ResolveUserLocation 翻译）。fail-closed：源不存在 → 未命中
// （调用方回落 404）；目标已存在 → 409（不静默覆盖）；checksum 不一致 → 409；封印 → 409。
func (o externalVolumeOps) ExternalRename(ctx context.Context, owner, volName, fromRemote, toRemote, expectedChecksum string) (bool, error) {
	h := o.h
	if h.volSet == nil {
		return false, nil
	}
	fsys, ok := h.externalFSFor(volName)
	if !ok {
		return false, nil
	}
	v, ok := h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return false, nil
	}
	srcLoc, srcErr := v.ResolveUserLocation(owner, fromRemote)
	dstLoc, dstErr := v.ResolveUserLocation(owner, toRemote)
	if srcErr != nil || dstErr != nil {
		return false, &files.HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	srcKey, dstKey := srcLoc.FSPath(), dstLoc.FSPath()
	if e, serr := fsys.Stat(ctx, srcKey); serr != nil || e == nil || e.IsDir {
		return false, nil // 源不存在/不可 stat → 未命中（404 由调用方裁决）
	}
	if e, derr := fsys.Stat(ctx, dstKey); derr == nil && e != nil {
		return true, &files.HTTPError{Status: http.StatusConflict, Message: "目标路径已存在"}
	}
	if cerr := crossCheckExternalChecksum(ctx, fsys, srcKey, expectedChecksum); cerr != nil {
		return true, cerr
	}
	return renameExternalChecked(ctx, h, fsys, volName, fromRemote, toRemote, srcKey, dstKey)
}

// renameExternalChecked 执行一次改名：原地改名优先；底层**明确不支持**（契约哨兵
// `syncpkg.ErrUnsupported`，如 secretdata 文件改 basename）→ 该卷设计口径的等价路径
// 「读源 → 写目标 → 删源」；封印 → 409；其余 → 500（如实失败，不静默半态）。
func renameExternalChecked(ctx context.Context, h *Handlers, fsys syncpkg.FS, volName, fromRemote, toRemote, srcKey, dstKey string) (bool, error) {
	rerr := fsys.Rename(ctx, srcKey, dstKey)
	switch {
	case rerr == nil:
		return true, nil
	case errors.Is(rerr, storage.ErrSealed):
		return true, &files.HTTPError{Status: http.StatusConflict, Message: "该路径由封装卷管理，禁止直接重命名"}
	case errors.Is(rerr, syncpkg.ErrUnsupported):
		if cerr := copyThenDeleteExternal(ctx, fsys, srcKey, dstKey); cerr != nil {
			h.logger.ErrorContext(ctx, "外部卷重命名（拷贝+删源）失败", "volume", volName,
				"from", fromRemote, "to", toRemote, "error", cerr.Error())
			return true, &files.HTTPError{Status: http.StatusInternalServerError, Message: "重命名失败"}
		}
		return true, nil
	default:
		h.logger.ErrorContext(ctx, "外部卷重命名失败", "volume", volName,
			"from", fromRemote, "to", toRemote, "error", rerr.Error())
		return true, &files.HTTPError{Status: http.StatusInternalServerError, Message: "重命名失败"}
	}
}

// copyThenDeleteExternal 以「读源 → 写目标 → 删源」实现底层不支持原地改名的外部卷文件改名
// （secretdata 的文件 basename 锚定于加密 meta ⇒ `Rename` 返回 `syncpkg.ErrUnsupported`）。
// 流式拷贝（内存有界，经同一可信链 ⇒ 目标落 sidecar/校验语义与普通写一致）；**写成功后才删源**
// ⇒ 崩溃窗口只会留下双份（fail-safe，不丢数据）。目标已存在由调用方前置 Stat 拒绝（不覆盖）。
func copyThenDeleteExternal(ctx context.Context, fsys syncpkg.FS, srcKey, dstKey string) error {
	e, serr := fsys.Stat(ctx, srcKey)
	if serr != nil {
		return serr
	}
	rc, oerr := fsys.OpenRead(ctx, srcKey)
	if oerr != nil {
		return oerr
	}
	werr := fsys.WriteFile(ctx, syncpkg.LogicalKey{}, dstKey, rc, e.Size, e.MTime)
	_ = rc.Close()
	if werr != nil {
		return werr
	}
	return fsys.Delete(ctx, srcKey)
}

// crossCheckExternalChecksum 用 sidecar **记录**的整文件 SHA-256 交叉比对客户端声明的
// checksum（不重算——远端整文件回读不可接受）。`expectedChecksum` 为空或 sidecar 缺失/不可读
// （无记录值可比）→ 跳过该判据，删除继续；不一致 → `*HTTPError{409}`。
func crossCheckExternalChecksum(ctx context.Context, fsys syncpkg.FS, key, expectedChecksum string) error {
	if expectedChecksum == "" {
		return nil
	}
	pv, ok := fsys.(meta.Provider)
	if !ok {
		return nil
	}
	fm, ferr := pv.FileMeta(ctx, key)
	if ferr != nil || fm == nil || fm.TotalSHA256 == "" || fm.TotalSHA256 == expectedChecksum {
		return nil
	}
	return &files.HTTPError{Status: http.StatusConflict, Message: "文件已变更（校验和不一致），请刷新后重试"}
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
	// P0-2 修复：下载路径也必须 Wrap（写侧才能落 sidecar、读侧才能经 meta.Provider
	// 逐分块校验）；此前只 Guard 导致 fileMetaReaderFor 恒 nil、外部卷下载零校验。
	return trusted.Guard(trusted.Wrap(fsys, h.trustedWrapOpts())), fsys != nil
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
// 到达即建配套 .meta（隐藏、占配额）。meta 恒生成（用户裁定：只关校验不关 meta——
// skip_verify 只放宽读侧校验）。
// （缺省 false = 启用）。WriteMeta 从已落盘文件计算 FileMeta（总/分块 sha256+md5）
// 并原子写 `.meta` sidecar 到 **meta 功能桶**（`meta/<rel>.meta`，移出 user/ 桶——
// 用户裁定 2026-10-07：杜绝与用户真实 `.meta` 文件名冲突），配额记入 owner 的
// meta 桶子 Scope（与主文件同账本体系，防磁盘超配额）。
type filesMetaPolicy struct{ h *Handlers }

var _ files.FileMetaPolicy = filesMetaPolicy{}

// Enabled 恒 true（用户裁定：meta 恒生成——只关校验不关 meta；sidecar 是完整性证据，
// 写入路径必落。skip_verify 只关读侧校验，不关本写侧 meta 生成）。
func (p filesMetaPolicy) Enabled() bool { return true }

// VerifyDownload 返回下载流的逐分块校验包装（信任保证演进：普通下载读回比对 meta，
// 异常 fail-closed——跨信任边界静默损坏在下发客户端前被拦截）。
// 装配层未注入 / skip_verify（显式放宽） / meta sidecar 缺失 → 返回 nil（直通零回归）。
// 实现：读 meta/<rel>.meta（root 相对）→ meta.VerifyReadSeeker 包装 r。
func (p filesMetaPolicy) VerifyDownload(ctx context.Context, root *storage.Root, rel string, r files.SeekReadCloser) (files.SeekReadCloser, error) {
	if p.h.verifySkipped() {
		return nil, nil // 显式跳过下载校验（极端性能场景；meta 仍恒生成）
	}
	if root != nil && root.IsEncrypted() {
		// 加密卷：sidecar 由 computeMeta 走 root.Open（密文）算，而本路径的流是明文
		// （OpenDecrypted），二者必然失配。加密卷完整性由卷内 GCM 认证保证——与
		// chunked_download.go 同口径跳过。
		return nil, nil
	}
	mrel := meta.MetaPath(rel)
	rc, err := root.Open(mrel)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// C2（P0）：sidecar **存在但读失败** → fail-closed，绝不直通。否则「后端同时
			// 坏 sidecar + 数据」时损坏内容静默下发（「异常 fail-closed」承诺仅对 meta
			// 完好时成立 = 信任承诺瓦解）。运营上如需彻底关闭校验，用既有的
			// `trusted_volume.skip_verify` 开关（显式、可观测）。
			return nil, fmt.Errorf("meta: sidecar 读取失败（存在但不可读，拒绝下发未校验内容）: %w", err)
		}
		// 真·sidecar 缺失（旁路写/未启用/存量数据）→ 直通：**本路径不提供任何服务端校验**，
		// **不重算 meta、不校验**。可观测性（第 5 轮对抗评审 P1）：升级后存量文件全部无 sidecar，
		// 运维必须能区分「无校验能力」与「校验通过」；用 Debug 避免大目录刷屏。
		p.warnDegradeDebug("sidecar 缺失", rel, err)
		return nil, nil
	}
	defer rc.Close()
	raw, rerr := io.ReadAll(io.LimitReader(rc, meta.MaxMetaSidecarBytes()+1))
	if rerr != nil {
		// C2（P0）：**有 sidecar 但读不完整** → 拒绝（不得静默直通）。
		p.warnDegrade("读取失败", rel, rerr)
		return nil, fmt.Errorf("meta: sidecar 读取失败（拒绝下发未校验内容）: %w", rerr)
	}
	if int64(len(raw)) > meta.MaxMetaSidecarBytes() {
		// 显式超限拒绝（与 trusted_fs.FileMeta 同口径）：截读会得到非法 JSON →
		// 静默直通；此处拒绝并告警，防被注入超大 JSON 的 OOM DoS（P2 对抗评审）。
		p.warnDegrade("大小超限", rel, nil)
		return nil, fmt.Errorf("meta: sidecar 超过上限 %d 字节（拒绝下发未校验内容）", meta.MaxMetaSidecarBytes())
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		// C2（P0）：**sidecar 损坏（解析/校验失败）** → 拒绝。损坏的 sidecar 意味着无法
		// 提供任何完整性证据——直通等于静默放弃承诺；报文可观测（Warn + 错误返回）。
		// m9：损坏 sidecar 的常见成因是「写 meta 崩溃于 rename 前」——同目录 `.tmp.*`
		// 孤儿一并清理（best-effort），避免孤儿常驻 meta 桶占配额。
		p.sweepMetaTmp(root, mrel)
		p.warnDegrade("解析失败", rel, uerr)
		return nil, fmt.Errorf("meta: sidecar 损坏（拒绝下发未校验内容）: %w", uerr)
	}
	// **崩溃窗口补偿**（第 5 轮对抗评审 ③）：主文件与 sidecar 是两次独立原子写，崩溃发生在
	// 「主文件已落新内容、sidecar 未更新」之间时会留下**陈旧 sidecar**（size 与实测内容长度
	// 不一致）。此处**提前到流下发之前**判定（VerifyReadSeeker 内部虽也判，但那时已在
	// ServeContent 流中，错误只能中断流），以便：
	//   1. 保留 C2 口径——**仍拒绝下发未校验内容**（不静默降级为「直通/未校验」，否则「内容
	//      被截断/追加」会被洗白成可信态，完整性承诺瓦解）；
	//   2. 与「sidecar 损坏」区分开：报 ErrStaleMeta + 专用 Warn（可告警/可分类），并给出
	//      可操作修复——重建 sidecar（重写一次该文件，或运维调 filesMetaPolicy.WriteMeta），
	//      不再表现为「无提示、永久不可读」。
	if size, serr := seekEndSize(r); serr == nil && size != fm.Size {
		p.warnStaleMeta(rel, size, fm.Size)
		return nil, fmt.Errorf("%w: 内容 %d B ≠ sidecar %d B（疑崩溃窗口留陈旧 sidecar；已拒绝下发未校验内容，重建 sidecar 后恢复）", meta.ErrStaleMeta, size, fm.Size)
	}
	// 外部卷下的 sidecar 与数据同存不受信后端：畸形 meta 一律由 VerifyReadSeeker 内部
	// 入口 Validate fail-closed（见 pkg/files/meta/verify.go），不在此处降级为直通。
	// 包装为逐分块校验流（ServeContent 消费，Seek 语义保持）。具体类型实现 Close——
	// 断言回 SeekReadCloser（底层 r 可 Close，包装透传）。
	v := meta.VerifyReadSeeker(r, fm)
	sr, ok := v.(files.SeekReadCloser)
	if !ok {
		return nil, nil // 防御：不可 Close 断言失败直通（正常不可达）
	}
	return sr, nil
}

// seekEndSize 探测流的内容长度（SeekEnd 后复原到起点）；不支持 Seek 时返回错误（调用方跳过判据）。
func seekEndSize(r io.Seeker) (int64, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, serr := r.Seek(0, io.SeekStart); serr != nil {
		return 0, serr
	}
	return end, nil
}

// warnStaleMeta 记录「陈旧 sidecar」专用告警：与普通降级告警可区分（可作告警规则），
// 并在文案里给出可操作修复（重建 sidecar），避免运维只看到「下载失败」而无从下手。
func (p filesMetaPolicy) warnStaleMeta(rel string, contentSize, metaSize int64) {
	if p.h == nil || p.h.logger == nil {
		return
	}
	p.h.logger.Warn("可信卷 sidecar 陈旧（内容长度与 FileMeta.Size 不一致）：已拒绝下发未校验内容；重建 sidecar 可恢复（重写该文件或调用 WriteMeta）",
		"path", rel, "content_bytes", contentSize, "meta_bytes", metaSize,
		"cause", "主文件/sidecar 两次独立原子写之间的崩溃窗口（或内容被截断/追加）")
}

// warnDegrade 记录「读侧校验被跳过」的可观测信号（与显式 skip_verify 可区分）。
func (p filesMetaPolicy) warnDegrade(reason, rel string, err error) {
	if p.h == nil || p.h.logger == nil {
		return
	}
	p.h.logger.Warn("可信卷读校验降级为直通", "reason", reason, "path", rel, "error", err)
}

// warnDegradeDebug 同 warnDegrade，但用 Debug 级别（仅用于「sidecar 缺失」这类升级
// 后对所有存量文件都成立、按 Warn 会刷屏的预期状态）。
func (p filesMetaPolicy) warnDegradeDebug(reason, rel string, err error) {
	if p.h == nil || p.h.logger == nil {
		return
	}
	p.h.logger.Debug("可信卷读校验降级为直通", "reason", reason, "path", rel, "error", err)
}

// WriteMeta 计算并写入配套 .meta（本地卷上传到达即建；失败返回错误由调用方 Warn 兜底，
// 读路径下发不依赖 meta（长度取 Stat），但 sidecar 缺失时不做校验）。
// 配额记账（C1 修复：覆盖写做 Adjust 差分、不重复累计）：
//   - 写前 stat 旧 sidecar 大小 prev——覆盖写（rename 替换旧 inode）时先释放旧 committed，
//     再 Commit 新字节（net = new - prev，杜绝 owner meta 桶 Scope 随覆盖次数通胀）；
//   - 删除联动处（deleteQuarantinedFile）按 sidecar 实际大小 ReleaseUsage（见 write_ops.go）。
func (p filesMetaPolicy) WriteMeta(ctx context.Context, owner string, root *storage.Root, rel string) error {
	mrel := meta.MetaPath(rel)
	if err := p.writeMetaCore(ctx, owner, root, rel, mrel); err != nil {
		// 统一补偿（对抗评审 P2）：任何失败分支都失效旧 sidecar——主文件已换新内容而
		// sidecar 仍描述旧内容会使读路径按旧分块校验恒失配（硬失败固化不可读，幂等重传
		// 不自愈）；删后退化为 missing → 读侧不校验（安全）。
		_ = root.Remove(mrel)
		return err
	}
	return nil
}

// writeMetaCore 是 WriteMeta 主体（计算 FileMeta → 配额预留 → 原子落盘）。
func (p filesMetaPolicy) writeMetaCore(ctx context.Context, owner string, root *storage.Root, rel, mrel string) error {
	// 计算 FileMeta：从已落盘文件（root 相对 rel）读取计算总 sha256+md5 + 分块。
	fm, err := p.computeMeta(ctx, root, rel)
	if err != nil {
		return err
	}
	data, err := meta.Marshal(fm)
	if err != nil {
		return err
	}
	// sidecar 落 meta 功能桶（`meta/<rel>.meta`）：与用户文件命名空间隔离。
	if dirErr := p.prepareMetaDir(root, mrel); dirErr != nil {
		return dirErr
	}
	// 写前 stat 旧 sidecar（覆盖写差分：prev 是旧 committed，先释放再落新）。
	prev := int64(0)
	if fi, serr := root.Stat(mrel); serr == nil && fi != nil {
		prev = fi.Size()
	}
	// 配额预留（owner 的 meta 桶 Scope）：不足 fail-closed（调用方 Warn 兜底，不落盘不超配额）。
	scope, reservation, err := p.reserveMetaQuota(owner, mrel, int64(len(data)))
	if err != nil {
		return err
	}
	defer func() {
		if reservation != nil {
			reservation.Release() // 仅未 Commit 时生效（Reservation 原子至多一次）
		}
	}()
	if err := p.atomicWriteMeta(root, mrel, data); err != nil {
		// C1 修复：覆盖写 meta 落盘失败 → 删旧 sidecar（best-effort）——残留描述旧
		// 内容的陈旧 meta 会使读路径按旧分块校验新内容恒失配（硬失败固化）；删后
		// 退化为 missing（读侧不校验）。
		// E-MAJOR 修复：**删旧 sidecar 时对称释放其已提交配额**（prev）——删除即释放
		// 磁盘字节，prev 仍占 meta 桶 Scope；否则磁盘释放但配额虚高（假 507），下次
		// 成功 WriteMeta prev=0 只 Commit 新字节，泄漏的 prev 永不复位（仅 reconcile 自愈）。
		_ = root.Remove(mrel)
		if scope != nil && prev > 0 {
			scope.ReleaseUsage(prev)
		}
		return err
	}
	// 配额落地：覆盖写先释放旧 committed（rename 替换旧 inode，旧字节不再占盘），
	// 再 Commit 新字节——差分 Adjust，防 owner meta 桶 Scope 随覆盖次数永久通胀。
	if scope != nil && prev > 0 {
		scope.ReleaseUsage(prev)
	}
	if reservation != nil {
		reservation.Commit(int64(len(data)))
		reservation = nil // 防止 defer Release 双放（Commit/Release 二选一）
	}
	return nil
}

// prepareMetaDir 确保 sidecar 目标父目录存在。
func (p filesMetaPolicy) prepareMetaDir(root *storage.Root, mrel string) error {
	dir := path.Dir(mrel)
	if dir == "." || dir == "" {
		return nil
	}
	return root.MkdirAll(dir, 0o755)
}

// reserveMetaQuota 在 owner 的 meta 桶 Scope 预留 sidecar 字节（不足 fail-closed）。
// 返回 (scope, reservation)：scope 供覆盖写差分（ReleaseUsage(prev)），reservation 供 Commit。
func (p filesMetaPolicy) reserveMetaQuota(owner, mrel string, size int64) (*quota.Scope, *quota.Reservation, error) {
	scope := p.h.quotaScopeFor(owner, mrel)
	if scope == nil {
		return nil, nil, nil // 无配额能力：不记账（与既有无配额部署一致）
	}
	res, rerr := scope.TryReserve(size)
	if rerr != nil {
		return nil, nil, rerr
	}
	return scope, res, nil
}

// atomicWriteMeta 复用存储层原子写（root 相对 + fsync + rename）：与主文件同语义。
// tmp 名带纳秒后缀防同 rel 并发写撞 O_EXCL（MINOR-2 修复）。
// m8 修复：写前清扫同 rel 旧 `.tmp.*` 孤儿（此前崩溃在 rename 前残留的 tmp 会常驻
// meta 桶占磁盘；下次 WriteMeta 自愈清理，防孤儿累积）。
func (p filesMetaPolicy) atomicWriteMeta(root *storage.Root, mrel string, data []byte) error {
	p.sweepMetaTmp(root, mrel)
	// **写前先删旧 sidecar**（评审 P2：崩溃窗口）。主文件与 sidecar 是两次独立原子写：覆盖写
	// 在两次写之间崩溃会留下「旧 sidecar + 新主文件」——读路径按旧分块恒失配 ⇒ 该文件**永久
	// fail-closed 500**（不自愈）。先删使该窗口退化为「无 sidecar」（读侧不校验/按 missing
	// 语义降级），宁可缺证据也不留**错证据**。删除失败不阻断（best-effort：后续 rename 会覆盖）。
	_ = root.Remove(mrel)
	tmpRel := mrel + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
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

// sweepMetaTmp 清理 mrel 同目录的旧 meta tmp 孤儿（崩溃残留）。best-effort：删除失败仅
// 日志语义（下次写再试）。
//
// 兼容三种真实命名（全部由代码生成，逐一核对）：
//   - `.tmp.<nano>`：本文件 atomicWriteMeta 的 tmp 名（`<base>.tmp.<unixNano>`）；
//   - `.<base>.tmp-*`：**LocalFS.writeFileAtomic** 经 `os.CreateTemp` 以 pattern
//     `"."+base+".tmp-*"` 生成的真实名（**带前导点**）——此前只匹配无前导点变体，导致这类孤儿实际**从未**被清理
//     （我的 m9 首版修复即此错：判据与生成端不一致 + fixture 用了假名 ⇒ 假绿）。
//
// **刻意不匹配** `<base>.tmp-*`（无前导点）：全仓无该命名的生成端，而它会误删合法 sidecar
// （如用户文件 `x.meta.tmp-abc` 的 sidecar `x.meta.tmp-abc.meta`）——宁漏不误删。
// metaTmpSweepThrottle 是 meta 孤儿清扫的**进程级节流窗口**。
//
// 为何节流（第 5 轮对抗评审 ⑥）：写路径每次写都调 sweepMetaTmp，而它要 ReadDir 整个 meta
// 目录——扁平大目录下 n 个文件各写一次 = O(n²) 目录枚举。孤儿是**惰性残留**（不阻断任何读写），
// 多存活≤窗口时长无副作用（下一次写/解析失败路径会清）。
var metaTmpSweepThrottle = time.Minute

// metaTmpSweepLast 是上次清扫时刻（unix nano；0 = 从未清扫）。进程级共享，
// 并发下可能两个 goroutine 同时扫（无害：Remove 幂等）。
var metaTmpSweepLast atomic.Int64

// metaTmpReadDir 是目录枚举入口（默认直连 root；测试可替换以断言「是否真的枚举了」。）
var metaTmpReadDir = func(root *storage.Root, dir string) ([]os.DirEntry, error) { return root.ReadDir(dir) }

// sweepMetaTmp 清理同目录内 .meta 写的 `.tmp` 孤儿（best-effort；失败静默——孤儿只占少量
// meta 桶配额，不阻断读写）。**节流**：同一进程内至多每 metaTmpSweepThrottle 枚举一次目录，
// 防写路径全量大目录 O(n²)（第 5 轮评审 ⑥）；孤儿多存活一个窗口属可接受降级。
func (p filesMetaPolicy) sweepMetaTmp(root *storage.Root, mrel string) {
	now := time.Now()
	if last := metaTmpSweepLast.Load(); last != 0 && now.Sub(time.Unix(0, last)) < metaTmpSweepThrottle {
		return
	}
	metaTmpSweepLast.Store(now.UnixNano())
	dir := path.Dir(mrel)
	bn := path.Base(mrel)
	prefixes := []string{bn + ".tmp.", "." + bn + ".tmp-"}
	es, err := metaTmpReadDir(root, dir)
	if err != nil {
		return // 目录不存在/不可读：无孤儿可清
	}
	for _, e := range es {
		if hasAnyPrefix(e.Name(), prefixes) {
			_ = root.Remove(path.Join(dir, e.Name()))
		}
	}
}

// hasAnyPrefix 报告 name 是否以任一前缀开头（sweep 判据的单一实现，防两处拷贝漂移）。
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// computeMeta 从 root 相对 rel 的文件计算完整 FileMeta（流式读一遍，双算法 + 分块）。
func (p filesMetaPolicy) computeMeta(ctx context.Context, root *storage.Root, rel string) (*meta.FileMeta, error) {
	// 用户可见语义：加密卷算**明文**分块/哈希（与读面同口径；否则 sidecar 存密文哈希
	// 在加密卷上无意义——读面按明文流无法用它校验，且跨面语义不一致）。
	rc, err := openUserVisibleRead(root, rel)
	if err != nil {
		return nil, fmt.Errorf("可信卷 meta 打开文件 %s: %w", rel, err)
	}
	defer rc.Close()
	stater, ok := rc.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return nil, fmt.Errorf("可信卷 meta 打开句柄不支持 Stat: %T", rc)
	}
	fi, serr := stater.Stat()
	if serr != nil {
		return nil, fmt.Errorf("可信卷 meta stat %s: %w", rel, serr)
	}
	// G3 修复：本地卷 sidecar 分块大小也接线 trusted_volume.chunk_size（此前硬编码 0
	// 自适应，同一文件在本地/外部卷得到不同 ChunkSize）。
	chunkSize := int64(0)
	if p.h.cfgPtr != nil {
		if cfg := p.h.cfgPtr.Load(); cfg != nil {
			chunkSize = int64(cfg.TrustedVolume.ChunkSize)
		}
	}
	c, cerr := meta.NewCalculator(fi.Size(), meta.ResolveChunkSize(chunkSize, fi.Size()))
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
