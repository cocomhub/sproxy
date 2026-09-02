// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
	"github.com/cocomhub/sproxy/web"
)

// Handlers 持有所有 HTTP handler 的依赖。
type Handlers struct {
	cfgPtr        *atomic.Pointer[Config]
	version       string
	buildAt       string
	tunnelHandler http.Handler
	// localHandler 是隧道内层本地文件 API handler（localMux + 中间件链，不含外层
	// 帧解密/密钥检查）。供 xfer listener（阶段 5 工作项 1）直接路由解密后的隧道
	// 请求；与 tunnelHandler（传统 POST /tunnel 外层帧解密器）互补。
	localHandler http.Handler
	logger       *slog.Logger
	metrics      *Metrics
	shareStore   *ShareStore
	routeTable   *hub.MeshRouteTable
	// dht 是节点发现表（nil = 不启用 DHT 候选，既有行为）。/api/hub/nodes 把 DHT
	// 候选节点合并进发现列表（路由表权威 + DHT 候选，去重）。由 cmd/sproxy 装配
	// Kademlia 时经 SetDHT 注入（hub.dht: kad）。
	dht hub.DHT
	// fedClient 是 hub 联邦节点表同步客户端（nil = 不启用联邦候选）。/api/hub/nodes
	// 把联邦候选节点合并进发现列表（路由表权威 + DHT + 联邦候选，去重）。由
	// cmd/sproxy 装配 hub.federation 时经 SetFederationClient 注入。
	fedClient *hub.FederationClient
	// relayStream 是 /api/relay/stream 处理器（RegisterRoutes 创建）。SetFederationClient
	// 注入联邦客户端时联动装配其跨 hub 转发器（路由表未命中 → 联邦转发）。
	relayStream *RelayStreamHandler
	// hubID 是本 hub 身份（config hub.node_id），跨 hub 转发防环路径记录用。
	hubID        string
	signalBroker *SignalBroker
	hubPersist   *hub.Persister // hub 状态持久化器（配置 hub.persist_file 时注入；nil = 不持久化）
	handler      http.Handler
	// auditLogger 是操作审计专用 logger：固定 JSON 格式（不随 log_format 切换）、
	// 与业务 logger 独立，保证审计行可机器检索。RegisterRoutes 初始化；测试可经
	// RegisterRoutesOpts.AuditLogger 注入 buffer 捕获。
	auditLogger    *slog.Logger
	cloudMgr       *CloudDownloadManager
	syncMgr        *syncmgr.Manager // 文件同步任务管理器（nil = 未配置 sync，相关路由返回 400）
	storageMgr     *StorageManager
	uploadingFiles sync.Map             // map[string]string — filename → uploadID，追踪正在上传的文件名
	uploadingStop  chan struct{}        // 关闭后通知 uploadingFiles 定期清理 goroutine 退出
	uploadingWg    sync.WaitGroup       // 等待 cleanupUploadingFilesLoop 退出
	closeOnce      sync.Once            // 防止 Close() 重复关闭 channel
	noncePool      *sproxysig.NoncePool // SproxySig nonce 防重放池

	// 多租户存储布局装配（任务 4，供 P2/P3 各 handler 迁移复用）。
	// globalRoot 是 OpenRoot 后的全局存储根（含 LAYOUT_VERSION 校验）；
	// globalPool 是全局配额池（cfg.MaxStorageBytes 兜底）。租户/checksum/配额均懒创建并缓存，
	// tenantMu 串行化懒创建（无竞态）；P2 各 handler 逐个切换到 tenantOf/checksumStoreFor/quotaFor。
	globalRoot     *storage.Root                      // 全局存储根（OpenRoot + LAYOUT_VERSION）
	globalPool     *quota.Pool                        // 全局配额池（cfg.MaxStorageBytes 兜底）
	tenantRoots    map[string]*storage.Tenant         // 按 owner 缓存租户（含 anonymous；懒创建）
	checksumStores map[string]*ChecksumStore          // 按 owner 缓存 per-tenant checksum 存储
	uploadStores   map[string]*UploadStore            // 按 owner 缓存 per-tenant 分块上传存储（懒创建）
	quotaScopes    map[string]*quota.Scope            // 按 owner 缓存配额 Scope（globalPool.Scope 懒创建）
	quotaBuckets   map[string]map[string]*quota.Scope // 按 owner 缓存功能桶配额子 Scope（user/cloud/archive/chunk/version）
	// archiveUsage 按 owner 登记已确认占用的归档文件（archive 桶），供删除时释放 Scope
	// （P5 审查重要 2：不依赖周期扫描自愈）。tenantMu 保护。
	archiveUsage map[string]map[string]int64
	tenantMu     sync.Mutex // 串行化 tenantRoots/checksumStores/uploadStores/quotaScopes/quotaBuckets/archiveUsage 懒创建
}

// TunnelUpdater 是隧道处理器密钥热替换接口。
// cmd/sproxy 的 SIGHUP 处理流程通过此接口在运行时替换隧道密钥。
type TunnelUpdater interface {
	UpdateKey(key []byte)
}

// SetFederationClient 注入 hub 联邦节点表同步客户端（nil 清除，恢复不合并联邦候选）。
// 由 cmd/sproxy 装配 hub.federation 时调用。
// 联动：同时装配 /api/relay/stream 的跨 hub 转发器（路由表未命中目标时，把 relay
// 拨号转发到上报该节点的联邦对端 hub）——节点表联邦合并与数据面联邦转发同步启用。
func (h *Handlers) SetFederationClient(fc *hub.FederationClient) {
	h.fedClient = fc
	if h.relayStream != nil {
		h.relayStream.SetFederation(fc, h.hubID)
	}
}

// SetSyncMgr 注入文件同步任务管理器（nil 清除，相关 /api/sync/* 路由返回 400）。
// 由 cmd/sproxy 在配置了 sync（sync.max_concurrent 或 sync_remotes）时调用。
func (h *Handlers) SetSyncMgr(mgr *syncmgr.Manager) {
	h.syncMgr = mgr
}

// TunnelHandler 返回隧道处理器，用于 SIGHUP 时热替换密钥。
func (h *Handlers) TunnelHandler() http.Handler {
	return h.tunnelHandler
}

// LocalHandler 返回隧道内层本地文件 API handler（localMux + 中间件链，
// 不含外层帧解密/密钥检查）。
//
// 供 xfer listener（阶段 5 工作项 1）直接路由解密后的隧道请求：xfer 隧道
// handleStream 已把请求体解密为明文，无需再经 TunnelHandler() 的外层帧解密
// （NewLocalHandler 期望请求 ctx 带派生密钥且 body 为帧协议——xfer 请求两者皆无，
// 直接使用会 401 unauthorized）。与 TunnelHandler() 互补：前者给传统 POST /tunnel，
// 后者给 xfer 隧道。
func (h *Handlers) LocalHandler() http.Handler {
	return h.localHandler
}

// anonymousOwner 是未认证请求的默认租户名（结构与其他租户完全同构）。
const anonymousOwner = "anonymous"

// normalizeOwner 把空 owner 归一为 anonymous 租户名（未认证请求的默认租户）。
func normalizeOwner(owner string) string {
	if owner == "" {
		return anonymousOwner
	}
	return owner
}

// ownerFromRequest 返回请求 ctx 中的操作主体（未认证返回 ""）。
func ownerFromRequest(r *http.Request) string {
	return ActorFrom(r.Context())
}

// tenantFor 返回 owner 的租户（空 owner → anonymous）。懒创建：首次访问按 owner 打开
// <storage_root>/<owner>/ 子根并建租户（ValidSegmentName fail-closed，非法返回 nil）；
// 已创建的缓存复用。globalRoot 未装配（或已关闭）时返回 nil。
//
// 注意：不预先为配置中所有 owner 建租户——首次请求时才建；anonymous 由 RegisterRoutes
// 启动时预创建（保证默认租户根存在）。
func (h *Handlers) tenantFor(owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if t, ok := h.tenantRoots[owner]; ok {
		return t
	}
	if h.globalRoot == nil {
		return nil
	}
	// fail-closed：owner 必须先过段名校验，再派生磁盘路径（防 Windows 保留字/穿越）。
	if !storage.ValidSegmentName(owner) {
		h.logger.Warn("非法租户名，拒绝创建（fail-closed）", "owner", owner)
		return nil
	}
	abs, ok := h.globalRoot.Abs(owner)
	if !ok {
		h.logger.Warn("租户路径越界，拒绝创建（fail-closed）", "owner", owner)
		return nil
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		h.logger.Warn("创建租户根目录失败", "owner", owner, "error", err)
		return nil
	}
	tenantRoot, err := storage.OpenRoot(abs)
	if err != nil {
		h.logger.Warn("打开租户子根失败（fail-closed）", "owner", owner, "error", err)
		return nil
	}
	t, err := storage.NewTenant(owner, tenantRoot)
	if err != nil {
		h.logger.Warn("创建租户失败（fail-closed）", "owner", owner, "error", err)
		_ = tenantRoot.Close()
		return nil
	}
	// 确保 meta 桶存在（供 per-tenant checksum / meta 记录写入）。
	if err := tenantRoot.MkdirAll("meta", 0o755); err != nil {
		h.logger.Warn("创建租户 meta 目录失败", "owner", owner, "error", err)
		_ = tenantRoot.Close()
		return nil
	}
	h.tenantRoots[owner] = t
	return t
}

// tenantOf 返回请求者 owner 的租户（owner 空 → anonymous 租户）。构造失败返回 nil
// （调用方按 400 处理，绝不回落全局根）。
func (h *Handlers) tenantOf(r *http.Request) *storage.Tenant {
	return h.tenantFor(ownerFromRequest(r))
}

// listTenantIDs 返回存储根下全部租户名（磁盘扫描，按名排序）。
// 供 CloudDownloadManager 恢复扫描使用：进程重启后内存缓存 tenantRoots 只有已访问的
// 租户（anonymous 预创建），仅靠缓存会漏掉已落盘但尚未访问的租户（如 alice 的云任务）。
// 磁盘扫描以租户根目录为准（跳过遗留 .__ 内部目录与非法段名目录）。
func (h *Handlers) listTenantIDs() []string {
	if h.globalRoot == nil {
		return nil
	}
	base, ok := h.globalRoot.Abs("")
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !storage.ValidSegmentName(name) {
			continue
		}
		// 跳过遗留服务端内部目录（.__ 魔法目录 / __ 遗留前缀等）——它们不是租户根。
		if strings.HasPrefix(name, ".__") || strings.HasPrefix(name, "__") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checksumStoreFor 返回 owner 的 per-tenant checksum 存储（懒创建，缓存到 map）。
// storePath = <tenant meta>/checksums.json；获取不到租户（非法 owner / 根不可用）返回 nil。
// P5 后不再有全局 checksum store——所有读写侧均经本方法取 per-tenant 实例。
func (h *Handlers) checksumStoreFor(owner string) *ChecksumStore {
	owner = normalizeOwner(owner)
	// 先取租户（内部锁 tenantMu，懒创建租户根 + meta 目录）。
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return nil
	}
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if cs, ok := h.checksumStores[owner]; ok {
		return cs
	}
	metaAbs, ok := tnt.Root().Abs("meta")
	if !ok {
		h.logger.Warn("派生租户 meta 路径失败", "owner", owner)
		return nil
	}
	cs := NewChecksumStore(filepath.Join(metaAbs, "checksums.json"), h.logger)
	h.checksumStores[owner] = cs
	return cs
}

// uploadStoreFor 返回 owner 的 per-tenant 分块上传存储（懒创建，缓存到 map）。
// 存储根 = 租户根下 chunk 桶（<root>/<owner>/chunk/，经 Tenant.Root().Abs("chunk")
// 派生绝对路径）。每租户独立 UploadStore 实例 → 会话天然物理隔离（会话目录
// <root>/<owner>/chunk/<uploadID>/），upload_id 无需 owner 前缀；跨租户同裸 id 互不可见。
// 获取不到租户（非法 owner / 根不可用）或创建失败返回 nil（调用方按 500/404 处理）。
func (h *Handlers) uploadStoreFor(owner string) *UploadStore {
	owner = normalizeOwner(owner)
	// 先取租户（内部锁 tenantMu，懒创建租户根）。
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return nil
	}
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if h.uploadStores == nil {
		h.uploadStores = make(map[string]*UploadStore)
	}
	if us, ok := h.uploadStores[owner]; ok {
		return us
	}
	chunkAbs, ok := tnt.Root().Abs("chunk")
	if !ok {
		h.logger.Warn("派生租户 chunk 路径失败", "owner", owner)
		return nil
	}
	cfg := h.cfgPtr.Load()
	var sessionTTL time.Duration
	if cfg != nil {
		sessionTTL = cfg.UploadSessionTTL
	}
	us, err := NewUploadStore(chunkAbs, sessionTTL, h.logger.With("component", "upload_store", "tenant", owner))
	if err != nil {
		h.logger.Error("创建 per-tenant UploadStore 失败", "tenant", owner, "error", err)
		return nil
	}
	// P5：quota 未装配（globalPool nil）时，分块上传走 storageMgr 回退预留，
	// 需把 storageMgr 注入 store 供会话删除/过期释放（scope 预留路径无需）。
	us.SetStorageMgr(h.storageMgr)
	h.uploadStores[owner] = us
	return us
}

// quotaBucketNames 是参与配额归集的功能桶名（对应租户根下的物理桶；meta 桶的配额
// 参与归集与否由任务 3 的扫描开启决定，此处先装配其子 Scope 供 reconciliation 使用）。
var quotaBucketNames = []string{"user", "cloud", "archive", "chunk", "version", "meta"}

// ensureTenantQuotaLocked 确保 owner 的租户配额 Scope、功能桶子 Scope 与 bucket_limits
// 路径子 Scope 已创建（调用方须持 tenantMu）。首次访问按 owner 在 globalPool 下挂载
// /tenant/<owner> Scope（上限 = cfg.OwnerQuotaFor(owner)），并预创建
// user/cloud/archive/chunk/version/meta 功能桶子 Scope（上限 0 = 不限制，租户上限由父
// Scope 单一执行），随后按 cfg.BucketLimits 对每个相对路径建精确路径子 Scope
// （scope.Scope(path, limit)，键即完整逻辑路径，供 quotaBucketFor 精确路径命中复用）。
// globalPool 未装配时返回 (nil, nil)。bucket_limits/owner_quotas 属装配期硬配置，
// 懒建后缓存不重建 → SIGHUP 后修改不生效（重启进程）。
func (h *Handlers) ensureTenantQuotaLocked(owner string) (*quota.Scope, map[string]*quota.Scope) {
	if s, ok := h.quotaScopes[owner]; ok {
		return s, h.quotaBuckets[owner]
	}
	if h.globalPool == nil {
		return nil, nil
	}
	if h.quotaScopes == nil {
		h.quotaScopes = make(map[string]*quota.Scope)
	}
	if h.quotaBuckets == nil {
		h.quotaBuckets = make(map[string]map[string]*quota.Scope)
	}
	var quotaBytes int64
	var bucketLimits map[string]int64
	if cfg := h.cfgPtr.Load(); cfg != nil {
		quotaBytes = cfg.OwnerQuotaFor(owner)
		bucketLimits = cfg.BucketLimits
	}
	s := h.globalPool.Scope("/tenant/"+owner, quotaBytes)
	buckets := make(map[string]*quota.Scope, len(quotaBucketNames)+len(bucketLimits))
	for _, b := range quotaBucketNames {
		buckets[b] = s.Scope(b, 0)
	}
	for path, limit := range bucketLimits {
		// 相对租户根路径（如 user/videos/hd）；加到功能桶之外的自定义子目录子 Scope。
		// 键即完整逻辑路径，quotaBucketFor 精确命中时复用（与功能桶子 Scope 同 map）。
		buckets[path] = s.Scope(filepath.ToSlash(path), limit)
	}
	h.quotaScopes[owner] = s
	h.quotaBuckets[owner] = buckets
	return s, buckets
}

// quotaFor 返回 owner 的 per-tenant 配额 Scope（懒创建，缓存到 map）。路径为
// /tenant/<owner>，上限 = cfg.OwnerQuotaFor(owner)（显式 owner > "*" 默认 > 0；
// anonymous 用 OwnerQuotaFor("anonymous")）。globalPool 未装配时返回 nil。
func (h *Handlers) quotaFor(owner string) *quota.Scope {
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	s, _ := h.ensureTenantQuotaLocked(owner)
	return s
}

// quotaBucketFor 返回 owner 租户下指定路径的配额子 Scope（懒创建，缓存复用）。
// 解析优先级（一级命中即返回）：
//  1. BucketLimits[path] 精确路径子 Scope（子目录/功能桶独立上限，如 user/videos/hd）；
//  2. 功能桶白名单子 Scope（quotaBucketNames：user/cloud/archive/chunk/version/meta）；
//  3. 否则 nil——任意的非白名单/未配置路径不建立任意子目录子 Scope。
//
// bucket 若不在功能桶白名单（如子目录路径），仅当它出现在 BucketLimits 里才返回对应
// 子 Scope；globalPool 未装配时返回 nil。写路径按物理桶归集：user 上传 → "user"、
// 分块 → "chunk"、云任务 → "cloud"、归档 → "archive"、版本 → "version"、meta → "meta"。
// 子 Scope 操作沿父链聚合到租户 Scope 与 globalPool（上限单一执行：精确路径命中时先
// 受该子 Scope 上限约束，再聚合到租户/全局）。
func (h *Handlers) quotaBucketFor(owner, bucket string) *quota.Scope {
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	_, buckets := h.ensureTenantQuotaLocked(owner)
	if buckets == nil {
		return nil
	}
	// 功能桶白名单键命中优先（保证既有语义），其次 BucketLimits 精确路径键。
	if sc, ok := buckets[bucket]; ok {
		return sc
	}
	// 任意的非白名单/未配置路径 → nil（不建立任意子目录子 Scope）。
	return nil
}

// RegisterRoutesOpts 是 RegisterRoutes 的选项参数结构体。
type RegisterRoutesOpts struct {
	Mux        *http.ServeMux
	CfgPtr     *atomic.Pointer[Config]
	Version    string
	BuildAt    string
	Logger     *slog.Logger
	RouteTable *hub.MeshRouteTable // 每 mesh 独立路由表的聚合（M-9）
	// HubPersist 是 hub 状态持久化器（配置 hub.persist_file 时由 flag 层注入）。
	HubPersist *hub.Persister
	// HubRestoredMessages 是启动时从持久化文件恢复的信令收件箱快照
	// （nodes 已在 routeTable 恢复；messages 需在 SignalBroker 创建后灌入队列）。
	HubRestoredMessages []hub.MessageSnap
	// AuditLogger 是操作审计专用 logger（nil 时默认固定 JSON 到 stdout）。
	// 独立于业务 Logger：格式不随 log_format 配置切换，审计行始终可机器检索。
	AuditLogger *slog.Logger
}

// RegisterRoutes 将所有 HTTP 路由注册到 mux 上，并返回 *Handlers。
// 调用方应在进程退出前调用 (*Handlers).Close() 以释放后台 goroutine 与持久化资源。
func RegisterRoutes(ctx context.Context, opts RegisterRoutesOpts) *Handlers {
	// TODO: ctx 当前未使用，后续可用于 graceful shutdown 或请求级超时控制
	srvMux := opts.Mux
	cfg := opts.CfgPtr.Load()
	log := defaultLogger(opts.Logger)
	// 用 WithContextHandler 包装：所有 InfoContext/DebugContext(ctx, ...) 日志
	// 自动读取 ctx 中的 SpanContext，带上 trace_id/span_id 实现全链路追踪。
	log = slog.New(tracing.WithContextHandler(log.Handler()))

	// 审计 logger：固定 JSON 格式（不随 log_format 切换），默认写 stdout 与业务
	// 日志同流；调用方可在 RegisterRoutesOpts.AuditLogger 注入自定义（如测试 buffer）。
	auditLogger := opts.AuditLogger
	if auditLogger == nil {
		auditLogger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	// 打开全局存储根（多租户布局：<storage_root>/<tenant>/{user,cloud,...}/）。
	// 目录不存在时先创建（原 storage_root 也是惰性创建）；storage.OpenRoot 会写入/校验
	// LAYOUT_VERSION。失败（目录无法打开 / 布局版本不匹配）是致命装配错误：记 Error 并
	// panic 拒绝启动，绝不静默继续（否则文件服务会在错误的存储根上运行）。
	storageRootPath := cfg.StorageRoot
	if err := os.MkdirAll(storageRootPath, 0o755); err != nil {
		log.Error("创建存储根目录失败", "path", storageRootPath, "error", err)
		panic("创建存储根目录失败: " + err.Error())
	}
	globalRoot, err := storage.OpenRoot(storageRootPath)
	if err != nil {
		log.Error("打开存储根失败（LAYOUT_VERSION 校验或目录不可用）", "path", storageRootPath, "error", err)
		panic("打开存储根失败: " + err.Error())
	}

	h := &Handlers{
		cfgPtr:        opts.CfgPtr,
		version:       opts.Version,
		buildAt:       opts.BuildAt,
		logger:        log,
		auditLogger:   auditLogger,
		metrics:       NewMetrics(),
		shareStore:    NewShareStore(log.With("component", "share")),
		routeTable:    opts.RouteTable,
		signalBroker:  NewSignalBroker(opts.RouteTable),
		hubPersist:    opts.HubPersist,
		hubID:         cfg.Hub.NodeID,
		uploadingStop: make(chan struct{}),
		noncePool:     sproxysig.NewNoncePool(),
	}
	// 装配多租户存储布局：全局配额池 + 懒创建缓存 + 预创建 anonymous 租户。
	// tenantRoots/checksumStores/uploadStores/quotaScopes 均为懒创建（首次请求时建），
	// 见 tenantFor/checksumStoreFor/uploadStoreFor 等辅助。
	h.globalRoot = globalRoot
	h.globalPool = quota.NewPool(cfg.MaxStorageBytes)
	h.tenantRoots = make(map[string]*storage.Tenant)
	h.checksumStores = make(map[string]*ChecksumStore)
	h.uploadStores = make(map[string]*UploadStore)
	h.quotaScopes = make(map[string]*quota.Scope)
	h.quotaBuckets = make(map[string]map[string]*quota.Scope)
	h.archiveUsage = make(map[string]map[string]int64)
	if h.tenantFor(anonymousOwner) == nil {
		// anonymous 是未认证请求的默认兜底租户，创建失败意味着存储根不可用，拒绝启动。
		log.Error("预创建 anonymous 租户失败，存储根不可用")
		panic("预创建 anonymous 租户失败")
	}
	// 预创建 anonymous 分块上传存储：/healthz 探活依赖它（healthz 检查 per-tenant store
	// 健康状态），同时保证未认证分块上传的 chunk 桶就绪。
	if h.uploadStoreFor(anonymousOwner) == nil {
		log.Error("预创建 anonymous UploadStore 失败，存储根不可用")
		panic("预创建 anonymous UploadStore 失败")
	}
	h.signalBroker.SetPersister(opts.HubPersist)

	// 启动时恢复持久化的信令收件箱（节点注册已在 cmd 层通过 RestoreFromSnapshot
	// 灌入 routeTable；此处把 messages 灌入 SignalBroker 队列，重启不丢待投递信令）。
	if len(opts.HubRestoredMessages) > 0 {
		hub.RestoreSignalQueue(h.signalBroker.queue, opts.HubRestoredMessages)
	}

	if h.routeTable != nil {
		// 节点注册/移除（Add/Remove/RemoveIfOwned）→ 持久化快照。onChange 回调要求
		// 快速返回（不做阻塞 I/O），故只排队（异步去抖落盘），真正写盘在 Persister 内部。
		h.routeTable.SetOnChange(func() {
			if opts.HubPersist == nil {
				return
			}
			opts.HubPersist.Schedule(func() *hub.Snapshot {
				snap := hub.SnapshotRouteTable(h.routeTable)
				// M4：与 FlushSignal 一致，用 signalSnapshots 过滤孤儿收件箱
				// （节点已不在路由表），避免 onChange 路径把死信写入持久化文件。
				snap.Messages = h.signalBroker.signalSnapshots()
				return snap
			})
		})
	}

	// 启动 uploadingFiles 定期清理 goroutine（OOM 防范）
	h.uploadingWg.Go(func() {
		h.cleanupUploadingFilesLoop()
	})

	// 初始化 StorageManager 和 CloudDownloadManager。
	// P4：StorageManager 保留全局账本（sync/旧装配兼容）；启动扫描经 SetReconciler 按租户桶
	// 归集校准 per-tenant 配额 Scope（重启后 Scope 不回溯）。云任务配额走 cloud 桶子 Scope。
	sm := NewStorageManager(cfg.StorageRoot, cfg.MaxStorageBytes, nil, log.With("component", "storage"))
	sm.SetReconciler(h.reconcileQuotaScopes)
	_ = sm.ScanAndRecalculate() // 装配后重扫：校准 per-tenant Scope（启动对账）
	cloudCfg := &CloudDownloadConfig{
		SyncThreshold:   cfg.CloudSyncThreshold,
		MaxConcurrent:   cfg.CloudMaxConcurrent,
		MaxBatchURLs:    cfg.CloudMaxBatchURLs,
		TaskTTL:         cfg.CloudTaskTTL,
		FailedTaskTTL:   cfg.CloudFailedTaskTTL,
		AllowPrivate:    cfg.CloudDownloadAllowPrivate,
		DownloadTimeout: cfg.CloudDownloadTimeout,
		IdleTimeout:     cfg.CloudDownloadIdleTimeout,
		MaxRetries:      cfg.CloudMaxRetries,
		RetryDelay:      cfg.CloudRetryDelay,
		Downloader:      cfg.CloudDownloader,
	}
	h.cloudMgr = NewCloudDownloadManager(cfg.StorageRoot, sm, h.tenantFor, h.checksumStoreFor, h.listTenantIDs, log.With("component", "cloud"), cloudCfg, func(owner string) *quota.Scope {
		return h.quotaBucketFor(owner, "cloud")
	})
	h.storageMgr = sm

	// 本地路由子 mux（无 authMiddleware，隧道密钥已提供认证）
	localMux := http.NewServeMux()
	localMux.HandleFunc("POST /upload", h.upload)
	localMux.HandleFunc("GET /download", h.download)
	localMux.HandleFunc("POST /delete", h.delete)
	localMux.HandleFunc("POST /rename", h.rename)
	localMux.HandleFunc("GET /api/files", h.listFiles)
	localMux.HandleFunc("HEAD /api/files/stat", h.stat)
	localMux.HandleFunc("POST /mkdir", h.mkdir)
	localMux.HandleFunc("POST /rmdir", h.rmdir)
	localMux.HandleFunc("GET /api/files/search", h.searchFiles)
	localMux.HandleFunc("POST /api/batch/delete", h.batchDelete)
	localMux.HandleFunc("POST /api/batch/rename", h.batchRename)

	localMux.HandleFunc("POST /api/archive", h.archiveHandler)
	localMux.HandleFunc("GET /api/archive-dir", h.archiveDirHandler)
	localMux.HandleFunc("GET /api/versions", h.listVersionsHandler)
	localMux.HandleFunc("POST /api/versions/restore", h.restoreVersionHandler)
	localMux.HandleFunc("DELETE /api/versions", h.deleteVersionHandler)
	localMux.HandleFunc("GET /api/stats", h.statsHandler)
	localMux.HandleFunc("GET /api/config", h.configHandler)
	localMux.HandleFunc("PUT /api/config", h.updateConfigHandler)

	// 分块上传/下载路由（本地）
	localMux.HandleFunc("POST /upload/init", h.uploadInit)
	localMux.HandleFunc("POST /upload/chunk", h.uploadChunk)
	localMux.HandleFunc("GET /upload/status", h.uploadStatus)
	localMux.HandleFunc("GET /upload/sessions", h.uploadSessions)
	localMux.HandleFunc("POST /upload/complete", h.uploadComplete)
	localMux.HandleFunc("GET /download/chunk", h.downloadChunk)

	// gzip + 速率限制 + CORS 中间件链
	var apiHandler http.Handler = localMux
	apiHandler = GzipMiddleware(log.With("component", "gzip"))(apiHandler)
	if cfg.RateLimit.Enabled {
		rl := NewRateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window, log.With("component", "rate_limiter"))
		apiHandler = rl.Middleware(apiHandler)
	}
	apiHandler = CORSMiddleware(cfg.CORS, log.With("component", "cors"))(apiHandler)

	// 隧道内层请求同样挂 requestLogMiddleware：解析客户端注入的 traceparent，
	// 生成子 span 并把 SpanContext 写入 ctx，使内层 handler 的 InfoContext/DebugContext
	// 日志带 trace_id/span_id，恢复隧道内层 per-request「收到/完成」日志。
	// 注意：这是 requestLogMiddleware 的第二个独立实例（主 mux 外层已用一次），
	// 对隧道路径独立生效，正确。
	// 隧道内层 handler：requestLogMiddleware（trace + 请求日志）包装 apiHandler。
	// localHandler 供 xfer listener 直接使用（请求体已解密为明文）；tunnelHandler
	// 是传统 POST /tunnel 的外层帧解密器（期望 ctx 带派生密钥 + body 为帧协议）。
	h.localHandler = h.requestLogMiddleware(apiHandler)
	h.tunnelHandler = tunnel.NewLocalHandler(nil, h.localHandler, log.With("component", "tunnel"))

	srvMux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	srvMux.HandleFunc("GET /download", h.authMiddleware(h.download))
	srvMux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
	srvMux.HandleFunc("POST /rename", h.authMiddleware(h.rename))
	srvMux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
	srvMux.HandleFunc("HEAD /api/files/stat", h.authMiddleware(h.stat))
	srvMux.HandleFunc("POST /upload/init", h.authMiddleware(h.uploadInit))
	srvMux.HandleFunc("POST /upload/chunk", h.authMiddleware(h.uploadChunk))
	srvMux.HandleFunc("GET /upload/status", h.authMiddleware(h.uploadStatus))
	srvMux.HandleFunc("GET /upload/sessions", h.authMiddleware(h.uploadSessions))
	srvMux.HandleFunc("POST /upload/complete", h.authMiddleware(h.uploadComplete))
	srvMux.HandleFunc("GET /download/chunk", h.authMiddleware(h.downloadChunk))
	srvMux.HandleFunc("POST /mkdir", h.authMiddleware(h.mkdir))
	srvMux.HandleFunc("POST /rmdir", h.authMiddleware(h.rmdir))
	srvMux.HandleFunc("GET /api/files/search", h.authMiddleware(h.searchFiles))
	srvMux.HandleFunc("POST /api/batch/delete", h.authMiddleware(h.batchDelete))
	srvMux.HandleFunc("POST /api/batch/rename", h.authMiddleware(h.batchRename))
	srvMux.HandleFunc("POST /api/archive", h.authMiddleware(h.archiveHandler))
	srvMux.HandleFunc("GET /api/archive-dir", h.authMiddleware(h.archiveDirHandler))
	srvMux.HandleFunc("GET /api/versions", h.authMiddleware(h.listVersionsHandler))
	srvMux.HandleFunc("POST /api/versions/restore", h.authMiddleware(h.restoreVersionHandler))
	srvMux.HandleFunc("DELETE /api/versions", h.authMiddleware(h.deleteVersionHandler))
	srvMux.HandleFunc("GET /api/stats", h.authMiddleware(h.statsHandler))
	srvMux.HandleFunc("GET /api/config", h.authMiddleware(h.configHandler))
	srvMux.HandleFunc("PUT /api/config", h.authMiddleware(h.updateConfigHandler))
	srvMux.HandleFunc("POST /api/share", h.authMiddleware(h.createShareHandler))
	srvMux.HandleFunc("GET /s/{token}", h.accessShareHandler)

	// 分享管理 API（localMux：隧道内部使用）
	localMux.HandleFunc("POST /api/share", h.createShareHandler)
	localMux.HandleFunc("GET /api/shares", h.listSharesHandler)
	localMux.HandleFunc("DELETE /api/shares/{token}", h.revokeShareHandler)

	// 分享管理 API（主 mux：Bearer auth）
	srvMux.HandleFunc("GET /api/shares", h.authMiddleware(h.listSharesHandler))
	srvMux.HandleFunc("DELETE /api/shares/{token}", h.authMiddleware(h.revokeShareHandler))

	// 云端下载 API（localMux：隧道认证）
	localMux.HandleFunc("POST /api/cloud/download", h.cloudCreateDownload)
	localMux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	localMux.HandleFunc("GET /api/cloud/tasks", h.cloudListTasks)
	localMux.HandleFunc("GET /api/cloud/tasks/{id}", h.cloudGetTask)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.cloudCancelTask)
	localMux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.cloudDeleteTask)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.cloudArchiveTask)
	localMux.HandleFunc("POST /api/cloud/archive", h.cloudArchiveBatch)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/resume", h.cloudResumeTask)
	localMux.HandleFunc("POST /api/cloud/groups", h.cloudCreateGroup)
	localMux.HandleFunc("GET /api/cloud/groups", h.cloudListGroups)
	localMux.HandleFunc("GET /api/cloud/groups/{id}", h.cloudGetGroup)
	localMux.HandleFunc("POST /api/cloud/groups/{id}/cancel", h.cloudCancelGroup)
	localMux.HandleFunc("DELETE /api/cloud/groups/{id}", h.cloudDeleteGroup)
	localMux.HandleFunc("POST /api/cloud/groups/{id}/resume", h.cloudResumeGroup)
	localMux.HandleFunc("POST /api/cloud/groups/{id}/archive", h.cloudArchiveGroup)
	// 云端下载 API（主 mux：Bearer auth）
	srvMux.HandleFunc("POST /api/cloud/download", h.authMiddleware(h.cloudCreateDownload))
	srvMux.HandleFunc("POST /api/cloud/download/batch", h.authMiddleware(h.cloudCreateBatchDownload))
	srvMux.HandleFunc("GET /api/cloud/tasks", h.authMiddleware(h.cloudListTasks))
	srvMux.HandleFunc("GET /api/cloud/tasks/{id}", h.authMiddleware(h.cloudGetTask))
	srvMux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.authMiddleware(h.cloudCancelTask))
	srvMux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.authMiddleware(h.cloudDeleteTask))
	srvMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.authMiddleware(h.cloudArchiveTask))
	srvMux.HandleFunc("POST /api/cloud/archive", h.authMiddleware(h.cloudArchiveBatch))
	srvMux.HandleFunc("POST /api/cloud/tasks/{id}/resume", h.authMiddleware(h.cloudResumeTask))
	srvMux.HandleFunc("POST /api/cloud/groups", h.authMiddleware(h.cloudCreateGroup))
	srvMux.HandleFunc("GET /api/cloud/groups", h.authMiddleware(h.cloudListGroups))
	srvMux.HandleFunc("GET /api/cloud/groups/{id}", h.authMiddleware(h.cloudGetGroup))
	srvMux.HandleFunc("POST /api/cloud/groups/{id}/cancel", h.authMiddleware(h.cloudCancelGroup))
	srvMux.HandleFunc("DELETE /api/cloud/groups/{id}", h.authMiddleware(h.cloudDeleteGroup))
	srvMux.HandleFunc("POST /api/cloud/groups/{id}/resume", h.authMiddleware(h.cloudResumeGroup))
	srvMux.HandleFunc("POST /api/cloud/groups/{id}/archive", h.authMiddleware(h.cloudArchiveGroup))

	// 文件同步 API（localMux：隧道认证；handler 在 syncMgr 未装配时返回 400）
	localMux.HandleFunc("POST /api/sync/tasks", h.syncCreateTask)
	localMux.HandleFunc("GET /api/sync/tasks", h.syncListTasks)
	localMux.HandleFunc("GET /api/sync/tasks/{id}", h.syncGetTask)
	localMux.HandleFunc("POST /api/sync/tasks/{id}/cancel", h.syncCancelTask)
	localMux.HandleFunc("DELETE /api/sync/tasks/{id}", h.syncDeleteTask)
	// 文件同步 API（主 mux：SproxySig auth）
	srvMux.HandleFunc("POST /api/sync/tasks", h.authMiddleware(h.syncCreateTask))
	srvMux.HandleFunc("GET /api/sync/tasks", h.authMiddleware(h.syncListTasks))
	srvMux.HandleFunc("GET /api/sync/tasks/{id}", h.authMiddleware(h.syncGetTask))
	srvMux.HandleFunc("POST /api/sync/tasks/{id}/cancel", h.authMiddleware(h.syncCancelTask))
	srvMux.HandleFunc("DELETE /api/sync/tasks/{id}", h.authMiddleware(h.syncDeleteTask))

	// Hub 管理 API（中继系统），需鉴权
	if opts.RouteTable != nil {
		// 任意 TCP 流中继（SSH/长连接）：升级为双向字节流。
		// 注：旧的 HTTP JSON 中继（POST /api/relay）已删除——被本流中继完全替代。
		// 仅支持直连（srvMux + Bearer）：handler 依赖 http.Hijacker 升级为原始 TCP，
		// 而隧道的 ResponseWriter 包装链（streamRecorder/gzipResponseWriter）不实现
		// Hijacker——经隧道访问必 500（旧版误注册到 localMux 的死路由，已删除）。
		streamHandler := NewRelayStreamHandler(opts.RouteTable, log.With("component", "relay_stream"))
		// 联邦转发器由 SetFederationClient 装配（注入联邦客户端时联动）；在此之前
		// 目标未本地命中即 404（与旧行为一致）。
		h.relayStream = streamHandler
		srvMux.HandleFunc("POST /api/relay/stream", h.authMiddleware(streamHandler.ServeHTTP))
		// TODO(I29)：若未来需要「经隧道做原始 TCP 中继」（链式中继/多跳），正确定位是
		// mux 层 raw-stream（复用 hub relay 模式），而非 http.Hijacker。见
		// .superpowers/sdd/i29-tunnel-hijack-value.md。

		// WebRTC 信令桥：SDP Offer/Answer/Candidate 存转 + 长轮询
		broker := h.signalBroker
		// S44：信令 POST 单独挂限流（独立实例，与文件传输隔离配额），防被攻破
		// 的已准入节点洪泛注入信令；GET poll 长轮询不挂（客户端高频轮询会误触发限流）。
		var signalPostRL *RateLimiter
		if cfg.RateLimit.Enabled {
			signalPostRL = NewRateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window, log.With("component", "signal_rate_limiter"))
		}
		signalPost := func(kind hub.SignalKind) http.HandlerFunc {
			hf := func(w http.ResponseWriter, r *http.Request) {
				broker.handleSignalPost(w, r, kind)
			}
			if signalPostRL == nil {
				return hf
			}
			return signalPostRL.Middleware(http.HandlerFunc(hf)).ServeHTTP
		}
		srvMux.HandleFunc("POST /api/signal/offer", h.authMiddleware(signalPost(hub.SignalOffer)))
		srvMux.HandleFunc("POST /api/signal/answer", h.authMiddleware(signalPost(hub.SignalAnswer)))
		// candidate 端点为 trickle ICE 预留注入点（当前 non-trickle 全内联 SDP，
		// 无生产 sender——保留兼容旧对端与未来增量，见 hub.SignalKind 注释）。
		srvMux.HandleFunc("POST /api/signal/candidate", h.authMiddleware(signalPost(hub.SignalCandidate)))
		srvMux.HandleFunc("GET /api/signal/poll/{peer}", h.authMiddleware(broker.handleSignalPoll))
		localMux.HandleFunc("POST /api/signal/offer", func(w http.ResponseWriter, r *http.Request) {
			broker.handleSignalPost(w, r, hub.SignalOffer)
		})
		localMux.HandleFunc("POST /api/signal/answer", func(w http.ResponseWriter, r *http.Request) {
			broker.handleSignalPost(w, r, hub.SignalAnswer)
		})
		localMux.HandleFunc("POST /api/signal/candidate", func(w http.ResponseWriter, r *http.Request) {
			broker.handleSignalPost(w, r, hub.SignalCandidate)
		})
		localMux.HandleFunc("GET /api/signal/poll/{peer}", broker.handleSignalPoll)

		srvMux.HandleFunc("GET /api/hub/nodes", h.authMiddleware(h.hubNodesHandler))
		srvMux.HandleFunc("DELETE /api/hub/nodes/{id}", h.authMiddleware(h.hubRemoveNodeHandler))
		srvMux.HandleFunc("GET /api/hub/stats", h.authMiddleware(h.hubStatsHandler))
		srvMux.HandleFunc("GET /api/hub/services", h.authMiddleware(h.hubServicesHandler))
		if cfg.Hub.Federation.Enabled {
			// 联邦节点表端点（hub-to-hub peering 入站面）：返回本 hub 路由表节点
			// （带 mesh），供对端 hub 周期拉取同步。走 authMiddleware（SproxySig
			// fail-closed：hub 配置 access_keys 后无凭据请求 401），不注册 localMux
			// （联邦是 hub 间直连 HTTP 同步，不经隧道）。
			srvMux.HandleFunc("GET /api/hub/federation/nodes", h.authMiddleware(h.federationNodesHandler))
		}
		// hub 用户面查询统一暴露 localMux：节点列表/统计/服务发现/移除在隧道内部
		// 均可调用（handler 按 routeTable==nil 返回 404 语义不变），保证浏览器隧道
		// 模式下 sclient.hub.* 全部可达；nodes/stats/remove 在 srvMux 侧仍是
		// authMiddleware 保护（直连面无降权）。本组在 opts.RouteTable != nil 内注册
		// （注册依赖 handler.signalBroker/routeTable 就位）。
		localMux.HandleFunc("GET /api/hub/nodes", h.hubNodesHandler)
		localMux.HandleFunc("DELETE /api/hub/nodes/{id}", h.hubRemoveNodeHandler)
		localMux.HandleFunc("GET /api/hub/stats", h.hubStatsHandler)
		localMux.HandleFunc("GET /api/hub/services", h.hubServicesHandler)
	}

	srvMux.HandleFunc("GET /healthz", h.healthz)
	srvMux.HandleFunc("GET /version", h.versionHandler)
	srvMux.HandleFunc("GET /metrics", h.MetricsHandler)
	// /tunnel 走 authMiddleware：SproxySig 验签成功后按 AK 查 SK 派生隧道密钥
	// （SetTunnelKey 放入 ctx），隧道 handler 用 ctx 密钥解密 metadata/body、加密响应。
	// 未验签的请求 401；隧道内层 localMux 请求（解密后转发）由隧道加密本身提供认证。
	srvMux.Handle("POST /tunnel", h.authMiddleware(http.HandlerFunc(h.tunnelHandler.ServeHTTP)))

	// Web UI
	subFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		h.logger.Error("web static fs sub error", "error", err)
	} else {
		fileServer := http.StripPrefix("/ui/", http.FileServer(http.FS(subFS)))
		srvMux.Handle("GET /ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:;")
			fileServer.ServeHTTP(w, r)
		}))
	}

	// GET / -> /ui/ 重定向。
	// 用 "{$}" 只精确匹配根路径：Go 1.22+ ServeMux 中 "GET /" 是 catch-all，
	// 会把任意未匹配路径（如 /foobar）也 301 到 /ui/；{$} 使未知路径返回 404。
	// （实测 /ui 无尾斜杠在 {$} 下返回 307 到 /ui/，浏览器自动跟随。）
	srvMux.HandleFunc("GET /{$}", h.webRedirect)

	h.handler = h.metricsMiddleware(h.requestLogMiddleware(srvMux))

	return h
}

// Close 释放 Handlers 持有的后台资源：停止 UploadStore 的 persist/cleanup goroutine 和 StorageManager 的定期扫描。
// 在进程退出前应调用一次（通常通过 defer h.Close()）。多次调用是安全的。
// 关闭顺序：先关 uploadingFiles 清理 goroutine，再关 UploadStore（后者可能还有 uploading 操作引用其 session）。
// TODO: 当前始终返回 nil；后续可收集各子组件关闭的错误，合并后返回。
func (h *Handlers) Close() error {
	// 先关闭 uploadingFiles 清理 goroutine，确保不再引用 uploadStore session
	h.closeOnce.Do(func() {
		close(h.uploadingStop)
	})
	h.uploadingWg.Wait()

	// 停止所有 per-tenant UploadStore（persist/cleanup goroutine）。
	// 保留 uploadStores map（不清空）：/healthz 探活需能看到已停止的 store 并返回 503；
	// Stop 幂等（stopOnce），重复 Close 安全。
	h.tenantMu.Lock()
	for _, us := range h.uploadStores {
		if us != nil {
			us.Stop()
		}
	}
	h.tenantMu.Unlock()

	if h.storageMgr != nil {
		h.storageMgr.Stop()
	}
	if h.cloudMgr != nil {
		h.cloudMgr.Close()
	}
	if h.shareStore != nil {
		h.shareStore.Stop()
	}
	if h.relayStream != nil && h.relayStream.forwarder != nil {
		h.relayStream.forwarder.Close()
	}
	// hub 状态持久化器最终 flush：优雅停服前把最后一次注册/信令变更落盘。
	// 快照生成在 Persister 锁内执行（FlushFn 持有 p.mu 再调 snapshotCurrent），
	// 避免停服时节点下线与快照生成之间的竞态导致旧快照覆盖新状态（I1）。
	if h.hubPersist != nil {
		if err := h.hubPersist.FlushFn(func() *hub.Snapshot { return h.snapshotCurrent() }); err != nil {
			h.logger.Error("shutdown: hub 状态最终落盘失败", "err", err)
		}
	}
	// 关闭多租户存储根：先关各租户子根（懒创建缓存），再关全局根。置 nil 防重复 Close。
	h.tenantMu.Lock()
	for _, tnt := range h.tenantRoots {
		if tnt != nil && tnt.Root() != nil {
			_ = tnt.Root().Close()
		}
	}
	h.tenantRoots = map[string]*storage.Tenant{}
	h.tenantMu.Unlock()
	if h.globalRoot != nil {
		_ = h.globalRoot.Close()
		h.globalRoot = nil
	}
	return nil
}

// snapshotCurrent 构建当前完整 hub 快照（节点 + 信令收件箱）。
// 命名不用 snapshotLocked：本函数自身不持任何锁（节点/队列锁在各 Snapshot 函数内
// 短临界区自行加解锁），避免误导调用方以为入参需预先持锁。
func (h *Handlers) snapshotCurrent() *hub.Snapshot {
	if h.routeTable == nil {
		return &hub.Snapshot{}
	}
	snap := hub.SnapshotRouteTable(h.routeTable)
	// M4：与 FlushSignal / onChange 一致，过滤孤儿收件箱，避免停服快照写入死信。
	snap.Messages = h.signalBroker.signalSnapshots()
	return snap
}

// Handler 返回包装了 metricsMiddleware 的 HTTP handler，用于 http.Server.Handler。
func (h *Handlers) Handler() http.Handler {
	return h.handler
}

func (h *Handlers) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, contentTypeTextPlain)
	// 探活 per-tenant UploadStore：任一已创建的 store 停止即判定不健康。
	h.tenantMu.Lock()
	stores := make([]*UploadStore, 0, len(h.uploadStores))
	for _, us := range h.uploadStores {
		if us != nil {
			stores = append(stores, us)
		}
	}
	h.tenantMu.Unlock()
	for _, us := range stores {
		if err := us.Health(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("UploadStore: " + err.Error()))
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (h *Handlers) versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, contentTypeTextPlain)
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "Version: %s\nBuildAt: %s\n", h.version, h.buildAt)
}

func (h *Handlers) webRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
}

// cleanupUploadingFilesLoop 定期清理 uploadingFiles 中已过期（不存在对应 session）的条目。
// 普通 upload 条目 value 为 "upload"（无 session），直接跳过。
// 作为 goroutine 在 RegisterRoutes 中启动，由 Close() 通过关闭 uploadingStop 停止。
func (h *Handlers) cleanupUploadingFilesLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.uploadingStop:
			return
		case <-ticker.C:
			h.uploadingFiles.Range(func(key, value any) bool {
				filename, ok := key.(string)
				if !ok {
					return true
				}
				uploadID, ok := value.(string)
				if !ok {
					return true
				}
				// 普通 upload 条目 value 为 "upload"，无对应 session，跳过
				if uploadID == "upload" {
					return true
				}
				// 分块上传条目 value 为 upload_id（裸 id）。uploadingFiles key 为
				// <tnt.ID>\x00<rel>（chunked init 与 upload handler 同格式），从 key 解析
				// 租户名取 per-tenant store 判断会话是否已不存在（则清理过期条目）。
				owner := ""
				if before, _, ok0 := strings.Cut(filename, "\x00"); ok0 {
					owner = before
				}
				if us := h.uploadStoreFor(owner); us != nil && us.GetSession(uploadID) == nil {
					h.uploadingFiles.Delete(filename)
				}
				return true
			})
		}
	}
}
