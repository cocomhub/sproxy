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
	"os"
	"path"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/internal/size"

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
	quota         QuotaScopeProvider
	ledger        ChecksumLedgerProvider
	chunkSizeFn   func() int64
	uploadLimit   func() int64
	versioning    Versioning
	chunked       ChunkedUploads
	downloadPaths DownloadPathResolver
	dedup         DedupPolicy
	locks         FileLocks
	metrics       Metrics
	audit         Auditor
	bandwidth     BandwidthLimiter
	eventSink     EventListener
	contentIndex  bool
	fileMeta      FileMetaPolicy
	externalOps   ExternalVolumeOps
	phys          VolumePhysFootprinter
	volTypes      VolumeTypeClassifier
	retrySleep    func(time.Duration)  // 外部卷转存重试退避 sleep（测试注入 0 等待）
	quotaWait     func() time.Duration // 配额排队等待上限（nil = quota.DefaultWaitTimeout 30s）
}

// waitTimeout 返回配额排队等待上限（注入的 fn 优先；nil = quota.DefaultWaitTimeout 30s）。
// 生产恒 DefaultWaitTimeout（语义不变）；测试经 WithQuotaWait 注入短值消除「故意打满断言 507」
// 用例的白等 30s。所有 WaitReserve 调用点必须走本方法（单一入口，防调用点漏改）。
func (rt runtime) waitTimeout() time.Duration {
	if rt.quotaWait != nil {
		if d := rt.quotaWait(); d > 0 {
			return d
		}
	}
	return quota.DefaultWaitTimeout
}

// contentIndexEnabled 返回内容索引开关（供索引容器构造时读取）。
func (rt runtime) contentIndexEnabled() bool { return rt.contentIndex }

// fileMetaEnabled 报告可信卷 meta 能力是否装配且启用（缺省 false 零回归）。
func (rt runtime) fileMetaEnabled() bool {
	return rt.fileMeta != nil && rt.fileMeta.Enabled()
}

// writeMetaSidecar 委托装配层实现写配套 .meta（本地卷上传到达即建；未装配 = 无操作）。
// owner 为文件归属（配额记账按 owner 的 meta 桶 Scope）；extra 为随上传写入的自定义
// 元数据（A/B/C 档；nil = 不写，零回归）。
func (rt runtime) writeMetaSidecar(ctx context.Context, owner string, root *storage.Root, rel string, extra map[string]any) error {
	if rt.fileMeta == nil {
		return nil
	}
	return rt.fileMeta.WriteMeta(ctx, owner, root, rel, extra)
}

// verifyDownload 委托装配层下载流逐分块校验（信任保证演进；未装配/无 meta/skip_verify
// → 返回 nil 直通——零回归）。
//
// 加密卷：sidecar 由 `computeMeta` 走 `root.Open`（**密文**）计算，而本路径的读取流是
// `root.OpenDecrypted`（**明文**）——两者必然失配，长度探测也因 `SeekEnd` 返回密文长度
// 而不报警。加密卷的完整性由卷内 GCM 认证保证，故与 `chunked_download.go` 同口径直接
// 跳过（否则加密卷 `/download` 恒被截断/客户端 unexpected EOF）。
func (rt runtime) verifyDownload(ctx context.Context, root *storage.Root, rel string, r SeekReadCloser) (SeekReadCloser, error) {
	if rt.fileMeta == nil {
		return nil, nil
	}
	if root != nil && root.IsEncrypted() {
		return nil, nil
	}
	return rt.fileMeta.VerifyDownload(ctx, root, rel, r)
}

// readMetaSidecar 委托装配层实现读本地卷 meta sidecar（`FileMetaPolicy.ReadMeta`）。
// 未装配 / 加密卷 / sidecar 缺失 → 空 map（读侧零校验语义）；存在但不可读/损坏 → 错误。
func (rt runtime) readMetaSidecar(ctx context.Context, root *storage.Root, mrel string) ([]byte, error) {
	if rt.fileMeta == nil || root == nil {
		return nil, os.ErrNotExist
	}
	if root.IsEncrypted() {
		return nil, os.ErrNotExist // 加密卷读侧口径不一致，与 VerifyDownload 同源跳过
	}
	return rt.fileMeta.ReadMetaRaw(ctx, root, mrel)
}

// sweepMetaTmp 清理 mrel 同目录的旧 meta tmp 孤儿（崩溃残留）。best-effort：删除失败
// 仅忽略（下次写同 rel 再试）；目录不可读无孤儿可清。删除联动调用（m4：文件删除后
// 孤儿常驻，删除路径也自愈）。
// 兼容三种真实命名（与 filesMetaPolicy.sweepMetaTmp 同口径）：
//   - `.tmp.<nano>`（atomicWriteMeta）；
//   - `.<base>.tmp-*`：LocalFS.writeFileAtomic 的真实名（**带前导点**）——此前判据缺该变体，
//     这类孤儿实际从未被清理（与 filesMetaPolicy 同型缺陷）。
//
// **刻意不匹配** `<base>.tmp-*`（无前导点）：无生成端且会误删合法 sidecar。——宁漏不误删。
func (rt runtime) sweepMetaTmp(root *storage.Root, mrel string) {
	dir := path.Dir(mrel)
	bn := path.Base(mrel)
	prefixes := []string{bn + ".tmp.", "." + bn + ".tmp-"}
	es, err := root.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range es {
		if hasAnyPrefix(e.Name(), prefixes) {
			_ = root.Remove(path.Join(dir, e.Name()))
		}
	}
}

// hasAnyPrefix 报告 name 是否以任一前缀开头（与 filesMetaPolicy 同实现）。
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
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
	svc := &Service{rt: newRuntime(tenants, cfg)}
	svc.indexForService()
	return svc, nil
}

// newRuntime 把 config 折叠为 runtime，并为未注入项装配默认实现。
func newRuntime(tenants TenantResolver, cfg config) runtime {
	rt := runtime{
		tenants:      tenants,
		loggerFn:     cfg.logger,
		actor:        cfg.actor,
		volumes:      cfg.volumes,
		quota:        cfg.quota,
		ledger:       cfg.ledger,
		chunkSizeFn:  cfg.chunkSize,
		uploadLimit:  cfg.uploadLimit,
		versioning:   cfg.versioning,
		chunked:      cfg.chunked,
		dedup:        cfg.dedup,
		locks:        cfg.locks,
		metrics:      cfg.metrics,
		audit:        cfg.audit,
		bandwidth:    cfg.bandwidth,
		eventSink:    cfg.eventSink,
		contentIndex: cfg.contentIndex,
		fileMeta:     cfg.fileMeta,
		externalOps:  cfg.externalOps,
		phys:         cfg.phys,
		volTypes:     cfg.volTypes,
		retrySleep:   cfg.retrySleep,
		quotaWait:    cfg.quotaWait,
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
	if rt.uploadLimit == nil {
		rt.uploadLimit = defaultUploadBodyLimit
	}
	if rt.versioning == nil {
		rt.versioning = disabledVersioning{}
	}
	if rt.dedup == nil {
		rt.dedup = disabledDedup{}
	}
	if rt.locks == nil {
		rt.locks = &mapFileLocks{}
	}
	rt.externalOps = cfg.externalOps
	rt.downloadPaths = cfg.downloadPaths
	if rt.downloadPaths == nil {
		rt.downloadPaths = defaultDownloadPaths{tenants: rt.tenants, actor: rt.actor}
	}
	return rt
}

// retrySleepFn 返回外部卷转存重试的退避等待函数（默认 time.Sleep；测试经 WithRetrySleep
// 注入 0 等待/假时钟）。访问器惰性取用（与带宽/日志同模式）。
func (r *runtime) retrySleepFn() func(time.Duration) {
	if r.retrySleep != nil {
		return r.retrySleep
	}
	return time.Sleep
}

func (r *runtime) bandwidthLimiter() BandwidthLimiter { return r.bandwidth }

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

// externalVolOps 返回外部卷对象操作接缝（未装配 → nil，调用方跳过外部卷分支 = 零回归）。
func (r *runtime) externalVolOps() ExternalVolumeOps { return r.externalOps }

// volTypeClassifier 返回卷类型分类能力（未注入 → nil，调用方回落内置基线名单）。
func (r *runtime) volTypeClassifier() VolumeTypeClassifier { return r.volTypes }

func (r *runtime) volumeTenant(volName, owner string) *storage.Tenant {
	return r.volumes.Tenant(volName, owner)
}

func (r *runtime) locateOwnerFile(owner, rel string) (FileLocation, bool) {
	return r.volumes.Locate(owner, rel)
}

// externalSinkFor 按 (owner, 卷名) 解析外部卷写入面（UploadSink）。
// 装配层经 VolumeSet 的 ExternalSinkSource 可选能力提供（filesVolumeSet.ExternalSink
// 委托 Handlers.externalSinkFor：trusted.Guard(Wrap) + staging 配额，与普通上传外部卷
// 同一条写面）。未装配 / 非外部卷 → nil（调用方 fail-closed）。
// **不做预留**（分块上传预留已在 init 由 Route 完成并挂在会话上）。
func (r *runtime) externalSinkFor(owner, volName string) UploadSink {
	vs := r.volumes.Volumes()
	if vs == nil {
		return nil
	}
	if es, ok := vs.(ExternalSinkSource); ok {
		return es.ExternalSink(owner, volName)
	}
	return nil
}

func (r *runtime) routeUpload(ctx context.Context, owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error) {
	return r.volumes.Route(ctx, owner, rel, explicitVol, size, forceHomeVol)
}

// routeTarget 为统一写内核显式卷定卷（RouteTarget：ACL + 双预留，不查重——覆盖写语义）。
func (r *runtime) routeTarget(ctx context.Context, owner, rel, volName string, size int64) (UploadRoute, error) {
	return r.volumes.RouteTarget(ctx, owner, rel, volName, size)
}

func (r *runtime) quotaScope(owner, rel string) *quota.Scope {
	if r.quota == nil {
		return nil
	}
	return r.quota.ScopeFor(owner, rel)
}

func (r *runtime) checksumStore(owner string) checksum.ChecksumStoreIface {
	if r.ledger == nil {
		return nil
	}
	return r.ledger.ChecksumStoreFor(owner)
}

func (r *runtime) chunkSize() int64 { return r.chunkSizeFn() }

// uploadBodyLimit 返回普通上传请求体上限（可配置；配置 <=0 回落 1 GiB 默认）。
func (r *runtime) uploadBodyLimit() int64 {
	if r.uploadLimit == nil {
		return size.UploadBodyLimit
	}
	if l := r.uploadLimit(); l > 0 {
		return l
	}
	return size.UploadBodyLimit
}

func (r *runtime) versioningEnabled() bool { return r.versioning.Enabled() }

func (r *runtime) versioningMaxVersions() int { return r.versioning.MaxVersions() }

func (r *runtime) versioningRetention() time.Duration { return r.versioning.Retention() }

// dedupEnabled 返回内容寻址去重是否启用（默认关闭）。
func (r *runtime) dedupEnabled() bool { return r.dedup != nil && r.dedup.DedupEnabled() }

// dedupStore 返回 owner 的 per-tenant 去重台账（未装配 → nil）。
func (r *runtime) dedupStore(owner string) *DedupStore {
	if r.dedup == nil {
		return nil
	}
	return r.dedup.DedupStoreFor(owner)
}

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

// publishFileEvent 推送文件变更事件（装配层 EventListener；nil = 不推送默认零回归）。
// rel 归一为相对 user 桶路径（去 user/ 前缀），与列表/索引 key 语义一致。
func (r *runtime) publishFileEvent(action, owner, rel string, size int64) {
	if r.eventSink == nil {
		return
	}
	rel = strings.TrimPrefix(rel, "user/")
	r.eventSink.OnFileEvent(action, owner, rel, size)
}

// updateMetaExtra 委托装配层旁路更新本地卷 sidecar 的 Extra（未装配 → ErrMetaUnavailable）。
// 模式经 ctx 承载（syncpkg.MetaUpdateModeFrom）。供 updateMetaLocal 调用。
func (rt runtime) updateMetaExtra(ctx context.Context, owner string, root *storage.Root, rel string, extra map[string]any) error {
	if rt.fileMeta == nil || !rt.fileMeta.Enabled() {
		return ErrMetaUnavailable
	}
	return rt.fileMeta.UpdateMetaExtra(ctx, owner, root, rel, extra)
}
