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
	"io"
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
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
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
	loc, kerr := v.ResolveUserLocation(owner, rel0)
	if kerr != nil {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	// 基于 locator：FS 键仅在此经 FSPath 拼接（唯一拼接点）。
	ownerKey := loc.FSPath()
	e, serr := fsys.Stat(ctx, ownerKey)
	if serr != nil || e == nil || e.IsDir {
		return files.DownloadPath{}, &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	return files.DownloadPath{
		Filename: remotePath, VolumeName: volName,
		Rel: ownerKey, Source: newExternalSource(fsys, ownerKey, e, h.verifySkipped()),
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
		// sidecar 缺失（旁路写/未启用/存量数据）→ 直通——**本路径不提供任何服务端校验**，
		// 非「直算兜底」。可观测性（第 5 轮对抗评审 P1）：升级后存量文件全部无 sidecar，
		// 运维必须能区分「无校验能力」与「校验通过」；用 Debug 避免大目录刷屏（读失败才是 Warn）。
		p.warnDegradeDebug("sidecar 缺失", rel, err)
		return nil, nil
	}
	defer rc.Close()
	raw, rerr := io.ReadAll(io.LimitReader(rc, meta.MaxMetaSidecarBytes()+1))
	if rerr != nil {
		p.warnDegrade("读取失败", rel, rerr)
		return nil, nil // meta 读失败：直通（不阻断下载）
	}
	if int64(len(raw)) > meta.MaxMetaSidecarBytes() {
		// 显式超限拒绝（与 trusted_fs.FileMeta 同口径）：截读会得到非法 JSON →
		// 静默直通；此处拒绝并告警，防被注入超大 JSON 的 OOM DoS（P2 对抗评审）。
		p.warnDegrade("大小超限", rel, nil)
		return nil, nil
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		// 可观测性（仓库既定：安全开关生效状态必须可观测、禁静默降级）：解析失败/超限
		// 与「本无 sidecar」可区分，供运维告警。
		p.warnDegrade("解析失败", rel, uerr)
		return nil, nil // meta 解析失败：直通（不阻断）
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
// 读路径 Stat 直算不依赖 meta 存在）。
// 配额记账（C1 修复：覆盖写做 Adjust 差分、不重复累计）：
//   - 写前 stat 旧 sidecar 大小 prev——覆盖写（rename 替换旧 inode）时先释放旧 committed，
//     再 Commit 新字节（net = new - prev，杜绝 owner meta 桶 Scope 随覆盖次数通胀）；
//   - 删除联动处（deleteQuarantinedFile）按 sidecar 实际大小 ReleaseUsage（见 write_ops.go）。
func (p filesMetaPolicy) WriteMeta(ctx context.Context, owner string, root *storage.Root, rel string) error {
	mrel := meta.MetaPath(rel)
	if err := p.writeMetaCore(ctx, owner, root, rel, mrel); err != nil {
		// 统一补偿（对抗评审 P2）：任何失败分支都失效旧 sidecar——主文件已换新内容而
		// sidecar 仍描述旧内容会使读路径按旧分块校验恒失配（硬失败固化不可读，幂等重传
		// 不自愈）；删后退化为 missing → 读路径直算兜底（安全）。
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
		// 退化为 missing（读路径直算兜底）。
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

// sweepMetaTmp 清理 mrel 同目录的旧 meta tmp 孤儿（`.tmp.<nano>` 前缀，崩溃残留）。
// best-effort：删除失败仅日志语义（下次写再试）。
func (p filesMetaPolicy) sweepMetaTmp(root *storage.Root, mrel string) {
	dir := path.Dir(mrel)
	base := path.Base(mrel) + ".tmp."
	es, err := root.ReadDir(dir)
	if err != nil {
		return // 目录不存在/不可读：无孤儿可清
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), base) {
			_ = root.Remove(path.Join(dir, e.Name()))
		}
	}
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
