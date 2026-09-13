// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// runtime.go 是文件服务的**已解析能力运行时**：把 Option 折叠成一组
// 能力接口 + 默认实现，并对领域内代码提供统一的 nil 安全访问器。
//
// 访问器是领域代码唯一的能力入口（处理器/存储/纯函数一律经 `s.rt.<accessor>()`），
// 这样「未装配某能力」的语义只在一处定义：
//   - 未注入配额/台账/审计/计量 → 返回 nil，各调用点按既有语义跳过；
//   - 未注入多卷 → singleVolume（由 TenantResolver 派生）；
//   - 未注入分块 → UploadStore 返回 nil（调用点已有的 nil 分支生效）；
//   - 未注入版本 → 关闭。
//
// 能力接口 + Option 的收益：不存在「取用函数 vs 快照值」的形状歧义，也不需要装配层的
// typed-nil 守卫（「未装配」由接口方法的 nil 返回表达）。

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// errTenantsRequired 是唯一必需能力缺失时的构造错误。租户解析决定请求落到哪个存储根，
// 没有它文件服务无法工作——按「必需项放编译期」的设计，它是 New 的第一个参数；此处兜底
// 显式传入 nil 接口的情况。
var errTenantsRequired = errors.New("files.New: TenantResolver 不能为 nil")

// runtime 是解析后的能力集合。零值不可用——必须经 New(tenants, opts...) 构造。
type runtime struct {
	tenants       TenantResolver
	loggerFn      func() *slog.Logger
	actor         ActorResolver
	volumes       VolumeRouter
	quota         QuotaScopes
	ledger        ChecksumLedgers
	chunkSizeFn   func() int64
	versioning    Versioning
	chunked       ChunkedUploads
	downloadPaths DownloadPaths
	locks         FileLocks
	metrics       Metrics
	audit         Auditor
}

// New 构造文件服务实例：**唯一必需项**是租户解析，其余能力由 Option 注入，未注入的
// 回落内建「最小可用」默认（单卷、无配额、无台账、无版本、无审计、无计量、内建锁池）。
//
//	svc, err := files.New(storage.NewTenantResolver("./data"))
//	svc, err := files.New(resolver, files.WithVolumes(vr), files.WithQuota(q))
func New(tenants TenantResolver, opts ...Option) (*Service, error) {
	if tenants == nil {
		return nil, errTenantsRequired
	}
	var cfg config
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &Service{rt: newRuntime(tenants, cfg)}, nil
}

// newRuntime 把 config 折叠为 runtime，并为未注入项装配默认实现。
func newRuntime(tenants TenantResolver, cfg config) runtime {
	rt := runtime{
		tenants:     tenants,
		loggerFn:    cfg.logger,
		actor:       cfg.actor,
		volumes:     cfg.volumes,
		quota:       cfg.quota,
		ledger:      cfg.ledger,
		chunkSizeFn: cfg.chunkSize,
		versioning:  cfg.versioning,
		chunked:     cfg.chunked,
		locks:       cfg.locks,
		metrics:     cfg.metrics,
		audit:       cfg.audit,
	}
	if rt.loggerFn == nil {
		rt.loggerFn = slog.Default
	}
	if rt.actor == nil {
		rt.actor = anonymousActor{}
	}
	if rt.volumes == nil {
		rt.volumes = singleVolume{tenants: tenants, quota: cfg.quota}
	}
	if rt.chunkSizeFn == nil {
		rt.chunkSizeFn = defaultChunkSize
	}
	if rt.versioning == nil {
		rt.versioning = disabledVersioning{}
	}
	if rt.locks == nil {
		rt.locks = &mapFileLocks{}
	}
	rt.downloadPaths = cfg.downloadPaths
	if rt.downloadPaths == nil {
		rt.downloadPaths = defaultDownloadPaths{tenants: rt.tenants, actor: rt.actor}
	}
	return rt
}

// ---- nil 安全访问器（领域代码唯一的能力入口） ----

func (r *runtime) logger() *slog.Logger { return r.loggerFn() }

func (r *runtime) actorOf(req *http.Request) string { return r.actor.Actor(req) }

// tenantOf 返回 owner 的租户。**空 owner 先归一为 anonymous**：未认证请求（Actor 返回 ""）
// 必须落到 anonymous 租户，而不是以「非法租户名」fail-closed 拒绝——这条策略属于领域本身
// （租户解析器只接收已归一的 owner，见 storage.TenantCache 的说明）。
func (r *runtime) tenantOf(owner string) *storage.Tenant {
	return r.tenants.TenantFor(normalizeOwner(owner))
}

func (r *runtime) volSet() VolumeSet { return r.volumes.Volumes() }

func (r *runtime) volumeTenant(volName, owner string) *storage.Tenant {
	return r.volumes.Tenant(volName, owner)
}

func (r *runtime) locateOwnerFile(owner, rel string) (FileLocation, bool) {
	return r.volumes.Locate(owner, rel)
}

func (r *runtime) routeUpload(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error) {
	return r.volumes.Route(owner, rel, explicitVol, size, forceHomeVol)
}

func (r *runtime) quotaScope(owner, rel string) *quota.Scope {
	if r.quota == nil {
		return nil
	}
	return r.quota.ScopeFor(owner, rel)
}

func (r *runtime) checksumStore(owner string) *checksum.ChecksumStore {
	if r.ledger == nil {
		return nil
	}
	return r.ledger.ChecksumStoreFor(owner)
}

func (r *runtime) chunkSize() int64 { return r.chunkSizeFn() }

func (r *runtime) versioningEnabled() bool { return r.versioning.Enabled() }

func (r *runtime) versioningMaxVersions() int { return r.versioning.MaxVersions() }

func (r *runtime) uploadStore(owner string) *UploadStore {
	if r.chunked == nil {
		return nil
	}
	return r.chunked.UploadStoreFor(owner)
}

func (r *runtime) storageManager() StorageManager {
	if r.chunked == nil {
		return nil
	}
	return r.chunked.Capacity()
}

func (r *runtime) fileLocks() FileLocks { return r.locks }

func (r *runtime) metricsRecorder() Metrics { return r.metrics }

func (r *runtime) resolveDownloadPath(req *http.Request) (DownloadPath, error) {
	return r.downloadPaths.Resolve(req)
}

func (r *runtime) recordFileAudit(ctx context.Context, action, object, result, detail string) {
	if r.audit == nil {
		return
	}
	r.audit.Record(ctx, action, object, result, detail)
}
