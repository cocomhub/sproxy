// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// options.go 定义文件服务的**能力接缝（消费者定义的原子接口）**与 **Option 构造模式**。
//
// 设计目标：
//   - **唯一必需项**：`New(tenants, opts...)` 只强制注入「租户解析」——没有它无处落盘；
//   - **默认即最小可用**：不注入任何 Option 也能完成单卷的
//     list/stat/download/upload/rename/delete/mkdir/rmdir/batch；
//   - **额外能力 = Option 注入接口**：多卷、配额、校验和台账、分块、版本、审计、计量、
//     文件锁、下载路径解析各为一个原子能力；
//   - **形状统一**：能力是接口，方法每次调用读实时状态，不存在「取用函数 vs 快照值」歧义；
//   - **typed-nil 消失**：接口字段的「未配置」用内部 nil 表达，装配层不再需要 `if x != nil`；
//   - **配置随能力走（C1）+ 可独立覆盖的配置项（C2）**：`ChunkedUploads` 自带容量与分块
//     大小；`WithChunkSize` 允许只覆盖分块大小。
//
// 函数适配器（tenantResolverFunc / actorResolverFunc / quotaScopesFunc / checksumLedgersFunc /
// downloadPathsFunc / auditorFunc）供「以单个函数表达一项能力」的装配与测试使用。

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// ---- 能力接口（消费者定义；实现由装配层或内建默认提供） ----

// TenantResolver 返回 owner 的租户（空 owner = anonymous）。这是**唯一必需**能力：
// 它决定请求落到哪个存储根。pkg/storage 提供可复用的默认实现（见 storage.NewTenantResolver）。
type TenantResolver interface {
	TenantFor(owner string) *storage.Tenant
}

// ActorResolver 从请求解析操作主体（未认证返回 ""）。默认：恒 anonymous。
type ActorResolver interface {
	Actor(*http.Request) string
}

// VolumeRouter 是**多卷能力**（运行时卷集合 + 卷租户 + 读定位 + 写路由）。
//
// 为什么是一个接口而不是多个字段：这四件事共同描述「多卷部署」这一个能力，拆开只会让
// 接缝出现「注入了 Volumes 却没注入 Route」的半配置组合。默认实现 `singleVolume` 由
// TenantResolver 派生（Volumes() 返回 nil = 单卷语义）。
type VolumeRouter interface {
	// Volumes 返回装配后的运行时卷集合；nil = 单卷（旧装配语义）。
	Volumes() VolumeSet
	// Tenant 返回指定卷上 owner 的租户（单卷默认实现委托 TenantResolver）。
	Tenant(volName, owner string) *storage.Tenant
	// Locate 在 owner 卷视图内定位 rel 所在卷（读定位，不创建目录）。
	Locate(owner, rel string) (FileLocation, bool)
	// Route 为写路径定卷并预留双账本（owner 全局 Scope + 卷容量池）。
	Route(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error)
}

// QuotaScopes 按文件相对路径解析最长前缀配额子 Scope；nil 实现 = 不记账。
type QuotaScopes interface {
	ScopeFor(owner, rel string) *quota.Scope
}

// ChecksumLedgers 返回 owner 的 per-tenant 校验和台账；nil 实现 = 不落台账（下载/stat 仍
// 实时计算 checksum 响应头）。
type ChecksumLedgers interface {
	ChecksumStoreFor(owner string) *checksum.ChecksumStore
}

// DownloadPaths 把下载/stat 请求解析为 (租户, 根内相对路径, 用户可见名)。默认实现只支持
// 普通文件；云端任务/归档等 kind 需装配层注入。
type DownloadPaths interface {
	Resolve(*http.Request) (DownloadPath, error)
}

// FileLocks 是文件级非阻塞互斥锁池（键 = 归一 owner + NUL + rel）。
//
// TryMark 以调用方给定的 value 占位（单次上传用锁标记、分块 init 用 upload_id）；
// Acquire 以实现的排他标记占位（delete / 版本 restore / 分块 complete）。两者共用同一键空间。
type FileLocks interface {
	TryMark(owner, rel, value string) (release func(), ok bool)
	Acquire(owner, rel string) (release func(), ok bool)
}

// ChunkedUploads 是**分块上传能力**（会话存储 + 可选的容量回退预留 + 分块大小）。
// 未注入 = 分块端点按当前 nil store 语义回包（不 panic）。
type ChunkedUploads interface {
	// UploadStoreFor 返回 owner 的 per-tenant 分块会话存储（懒建缓存由实现持有）。
	UploadStoreFor(owner string) *UploadStore
	// Capacity 返回容量回退预留目标（quota 未装配时的 P5 路径）；nil = 无回退预留。
	Capacity() StorageManager
}

// Versioning 是文件版本策略：是否启用 + 保留上限（<=0 = 不清理）。默认：关闭。
type Versioning interface {
	Enabled() bool
	MaxVersions() int
}

// Auditor 记录一条文件对象审计（action/object/result/detail）；nil 实现 = 不审计。
// ObjectType 固定为 file；actor/mesh/TS 由实现补齐。
type Auditor interface {
	Record(ctx context.Context, action, object, result, detail string)
}

// ---- Option 构造 ----

// Option 是文件服务的构造选项。零个 Option 即得到「最小可用」实例（单卷、无配额、
// 无台账、无版本、无审计、无计量、内建锁池）。
type Option func(*config)

// config 是 Option 的累积结果；零值字段在 newRuntime 中回落默认实现。
type config struct {
	logger        func() *slog.Logger
	actor         ActorResolver
	volumes       VolumeRouter
	quota         QuotaScopes
	ledger        ChecksumLedgers
	chunkSize     func() int64
	versioning    Versioning
	chunked       ChunkedUploads
	downloadPaths DownloadPaths
	locks         FileLocks
	metrics       Metrics
	audit         Auditor
}

// WithLogger 注入业务日志器访问器（取用函数；日志配置热更新需要每次读实时实例）。
// 默认 slog.Default。
func WithLogger(logger func() *slog.Logger) Option {
	return func(c *config) { c.logger = logger }
}

// WithActor 注入请求主体解析器。默认：恒 anonymous（所有操作落 anonymous 租户）。
func WithActor(a ActorResolver) Option {
	return func(c *config) { c.actor = a }
}

// WithVolumes 注入多卷能力。默认：由 TenantResolver 派生的单卷实现。
func WithVolumes(v VolumeRouter) Option {
	return func(c *config) { c.volumes = v }
}

// WithQuota 注入配额能力。默认：不记账。
func WithQuota(q QuotaScopes) Option {
	return func(c *config) { c.quota = q }
}

// WithChecksumLedger 注入校验和台账。默认：不落台账。
func WithChecksumLedger(l ChecksumLedgers) Option {
	return func(c *config) { c.ledger = l }
}

// WithDownloadPaths 注入下载路径解析（云端 kind 等）。默认：仅普通文件。
func WithDownloadPaths(d DownloadPaths) Option {
	return func(c *config) { c.downloadPaths = d }
}

// WithFileLocks 注入文件级锁池（需与外部 move/上传清理共用同一键空间时使用）。
// 默认：内建独立锁池。
func WithFileLocks(l FileLocks) Option {
	return func(c *config) { c.locks = l }
}

// WithChunkedUploads 注入分块上传能力（会话存储 + 容量回退预留）。默认：未装配。
func WithChunkedUploads(cu ChunkedUploads) Option {
	return func(c *config) { c.chunked = cu }
}

// WithChunkSize 覆盖分块大小（C2：独立可覆盖的纯配置项）。默认 internal/size.DefaultChunkSize。
func WithChunkSize(fn func() int64) Option {
	return func(c *config) { c.chunkSize = fn }
}

// WithVersioning 注入版本策略。默认：关闭（同名不同 checksum → 409）。
func WithVersioning(v Versioning) Option {
	return func(c *config) { c.versioning = v }
}

// WithAudit 注入文件对象审计。默认：不审计。
func WithAudit(a Auditor) Option {
	return func(c *config) { c.audit = a }
}

// WithMetrics 注入计量器。默认：不计量。
func WithMetrics(m Metrics) Option {
	return func(c *config) { c.metrics = m }
}

// ---- 内建默认实现（最小可用能力） ----

// anonymousActor 未注入请求主体时的默认：恒匿名（与未认证语义一致）。
type anonymousActor struct{}

func (anonymousActor) Actor(*http.Request) string { return "" }

// disabledVersioning 未注入版本策略时的默认：关闭、不清理。
type disabledVersioning struct{}

func (disabledVersioning) Enabled() bool    { return false }
func (disabledVersioning) MaxVersions() int { return 0 }

// fileLockTxnMarker 是文件级排他锁（delete / restore / complete）在锁池中的占位值。
// 与 pkg/server.uploadingLockTxn 同值（"txn"）：该字面量是跨层值契约——装配层的过期清理
// 按 isUploadingLockMarker 跳过锁条目（不把长事务持锁误判为过期会话）。
const fileLockTxnMarker = "txn"

// mapFileLocks 是内建锁池：非阻塞 sync.Map，键 = 归一 owner + NUL + rel。
// 零值可用（无需构造）。
type mapFileLocks struct {
	m sync.Map
}

func (l *mapFileLocks) TryMark(owner, rel, value string) (func(), bool) {
	key := normalizeOwner(owner) + "\x00" + rel
	if _, loaded := l.m.LoadOrStore(key, value); loaded {
		return nil, false
	}
	return func() { l.m.Delete(key) }, true
}

func (l *mapFileLocks) Acquire(owner, rel string) (func(), bool) {
	return l.TryMark(owner, rel, fileLockTxnMarker)
}

// singleVolume 是未注入多卷能力时的默认实现：唯一根、无卷 ACL、写路由恒默认租户。
// quota 非 nil 时写路由在 owner 全局 Scope 上做单账本预留（无卷容量池——单卷无卷池语义）。
type singleVolume struct {
	tenants TenantResolver
	quota   QuotaScopes
}

func (s singleVolume) Volumes() VolumeSet { return nil }

func (s singleVolume) Tenant(volName, owner string) *storage.Tenant {
	return s.tenants.TenantFor(owner)
}

// Locate 复刻单卷读定位语义（pkg/server.locateOwnerFile 的 h.volSet == nil 分支）：
// 唯一根下 stat 命中即定位成功（卷名空）。
func (s singleVolume) Locate(owner, rel string) (FileLocation, bool) {
	tnt := s.tenants.TenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return FileLocation{}, false
	}
	if _, err := tnt.Root().Stat(rel); err != nil {
		return FileLocation{}, false
	}
	return FileLocation{VolumeName: "", Tenant: tnt}, true
}

// Route 复刻单卷写路由语义（pkg/server.routeUpload 的 h.volSet == nil 分支）：恒默认租户；
// 装配了配额时在 owner 全局 Scope 上预留（单卷无卷容量池）；未装配时无预留。
// 租户不可用时 Tenant 为 nil，由 Upload 回 400；配额不足回 507。
func (s singleVolume) Route(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error) {
	route := UploadRoute{Tenant: s.tenants.TenantFor(owner)}
	if s.quota != nil {
		if scope := s.quota.ScopeFor(owner, rel); scope != nil {
			res, err := scope.TryReserve(size)
			if err != nil {
				return UploadRoute{}, &HTTPError{Status: http.StatusInsufficientStorage, Message: "存储配额不足"}
			}
			route.Scope = scope
			route.ScopeRes = res
		}
	}
	// Release 双回滚预留（闭包捕获 route 变量，读到赋值后的 ScopeRes）。
	route.Release = func() {
		if route.ScopeRes != nil {
			route.ScopeRes.Release()
		}
	}
	return route, nil
}

// defaultDownloadPaths 未注入下载路径解析时的默认：仅普通文件（kind 为空）。
// 云端任务/归档 kind 需要装配层注入（依赖跨族能力）。
type defaultDownloadPaths struct {
	tenants TenantResolver
	actor   ActorResolver
}

func (d defaultDownloadPaths) Resolve(r *http.Request) (DownloadPath, error) {
	if kind := r.URL.Query().Get("kind"); kind != "" {
		return DownloadPath{}, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	name := r.URL.Query().Get("filename")
	remotePath, err := pathguard.ValidateFilePath(name)
	if err != nil {
		if name == "" {
			return DownloadPath{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgEmptyFilename}
		}
		return DownloadPath{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidFilename}
	}
	tnt := d.tenants.TenantFor(d.actor.Actor(r))
	if tnt == nil || tnt.Root() == nil {
		return DownloadPath{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		return DownloadPath{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	return DownloadPath{Filename: remotePath, Tenant: tnt, Rel: rel}, nil
}

// ---- 函数适配器（以单个函数表达一项能力；供装配与测试使用） ----

// tenantResolverFunc 以函数适配 TenantResolver。
type tenantResolverFunc func(owner string) *storage.Tenant

func (f tenantResolverFunc) TenantFor(owner string) *storage.Tenant { return f(owner) }

// actorResolverFunc 以函数适配 ActorResolver。
type actorResolverFunc func(*http.Request) string

func (f actorResolverFunc) Actor(r *http.Request) string { return f(r) }

// quotaScopesFunc 以函数适配 QuotaScopes。
type quotaScopesFunc func(owner, rel string) *quota.Scope

func (f quotaScopesFunc) ScopeFor(owner, rel string) *quota.Scope { return f(owner, rel) }

// checksumLedgersFunc 以函数适配 ChecksumLedgers。
type checksumLedgersFunc func(owner string) *checksum.ChecksumStore

func (f checksumLedgersFunc) ChecksumStoreFor(owner string) *checksum.ChecksumStore { return f(owner) }

// downloadPathsFunc 以函数适配 DownloadPaths。
type downloadPathsFunc func(*http.Request) (DownloadPath, error)

func (f downloadPathsFunc) Resolve(r *http.Request) (DownloadPath, error) { return f(r) }

// auditorFunc 以函数适配 Auditor。
type auditorFunc func(ctx context.Context, action, object, result, detail string)

func (f auditorFunc) Record(ctx context.Context, action, object, result, detail string) {
	f(ctx, action, object, result, detail)
}

// defaultChunkSize 返回内建默认分块大小。
func defaultChunkSize() int64 { return size.DefaultChunkSize }
