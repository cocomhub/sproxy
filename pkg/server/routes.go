// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// routes.go 是**路由注册面**：RegisterRoutesOpts 选项结构体、RegisterRoutes（全部 HTTP 端点
// 的装配与挂载，含主 mux 的 SproxySig 认证中间件与 localMux 隧道内层路由），以及路由级辅助
// （isFileGroupedRoute / localMuxGate / fileRoute）。
//
// 拆分说明见 handlers.go 顶部。

package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/web"
)

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
	// Tracer 是请求路径的可选 telemetry.Tracer（nil = 不接通，requestLogMiddleware
	// 保持自生成 trace/span id 的既有行为；非 nil = 请求 span 由该 tracer 建立，
	// 如 ext/otel 经 provider 装配的 OTel tracer）。由 cmd/sproxy 在
	// telemetry.enabled=true 时注入，打通 OTel ↔ slog 日志链路。
	Tracer telemetry.Tracer
	// AllowInsecureLoopback 是测试专用瞬态覆盖：默认 false；测试注入空 Ring 时经
	// RegisterRoutesOpts 一并设为 true（等价旧 --allow-no-auth 全放行调试语义），
	// 使无凭据测试直连被兜底放行。为一次性读取（不写入 cfg，避免 SIGHUP/cfgPtr
	// 并发覆盖污染；多测试并发各用独立 RegisterRoutes，无竞态）。
	AllowInsecureLoopback bool
	// CredentialRing 是 SproxySig 凭据表（Ring）。nil 时 RegisterRoutes 自动装配：
	// 从 CredentialStore 载入；仍空则 **U3 零凭据启动**——不生成 anonymous，系统以
	// 零凭据等待注册（register 公开端点唯一入口，首位回环注册者原子授 admin）。
	// 测试/xfer 集成可显式注入（配合 AllowInsecureLoopback）。
	CredentialRing *accesskey.Ring
	// CredentialStore 是凭据 store（nil = 不载入/不持久化，纯内存 Ring 场景，
	// 如注入空 Ring 的无认证测试）。持 accesskey.CredentialStorer（接口提取后
	// 外部可注入 KMS 等 storer 实现；*CredentialStore 自动满足）。
	CredentialStore accesskey.CredentialStorer
	// Authenticators 是认证面插件化宿主嵌入点（DEC-C，R3-I1/I2）：非 nil →
	// **replace 默认链**（宿主全权掌控，需含 RingAuthenticator 则自行加入）；nil →
	// 默认装配 []Authenticator{RingAuthenticator{...}}。宿主可注入自有实现（映射
	// 自有用户/会话 → Principal），文件操作按 Principal.AK 落桶（宿主把目标桶 ID
	// 放入 Principal.AK，保持 4A 按 AK 落桶现状零回归）。
	Authenticators []Authenticator
	// TotpRateLimit 是 totp_limiter（nonce/login 共用）的测试专用瞬态覆盖（每分钟
	// 请求数；0 = 默认 10/min）。与 AllowInsecureLoopback 同为一次性读取的测试瞬态，
	// 不写入 cfg（避免 SIGHUP/cfgPtr 并发覆盖污染）。
	TotpRateLimit int
	// LoginRateLimit 是 login_limiter 的测试专用瞬态覆盖（每分钟请求数；
	// 0 = 默认 10/min）。同 TotpRateLimit 语义，供登录黑盒测试避免过早限流。
	LoginRateLimit int
}

// RegisterRoutes 将所有 HTTP 路由注册到 mux 上，并返回 *Handlers。
// 调用方应在进程退出前调用 (*Handlers).Close() 以释放后台 goroutine 与持久化资源。
func RegisterRoutes(ctx context.Context, opts RegisterRoutesOpts) *Handlers {
	// ctx 当前未使用（保留在签名里：装配层已按进程生命周期注入，将来用于 graceful shutdown
	// 或请求级超时控制时无需改所有调用点）。注意请求级上下文在各 handler 内是 r.Context()。
	srvMux := opts.Mux
	cfg := opts.CfgPtr.Load()
	log := slogutil.Default(opts.Logger)
	// 用 WithContextHandler 包装：所有 InfoContext/DebugContext(ctx, ...) 日志
	// 自动读取 ctx 中的 SpanContext，带上 trace_id/span_id 实现全链路追踪。
	log = slog.New(telemetry.WithContextHandler(log.Handler()))

	// 审计 logger：固定 JSON 格式（不随 log_format 切换），默认写 stdout 与业务
	// 日志同流；调用方可在 RegisterRoutesOpts.AuditLogger 注入自定义（如测试 buffer）。
	auditLogger := opts.AuditLogger
	if auditLogger == nil {
		auditLogger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	// 有界内存环形审计缓冲：按 cfg.Audit.BufferSize 装配（默认 2048 由 SetDefaults
	// 保证；显式 0 = 关闭，auditRing 保持 nil，GET /api/audit 返回空表）。
	var auditRing *AuditRing
	if cfg.Audit.BufferSize > 0 {
		auditRing = NewAuditRing(cfg.Audit.BufferSize)
	}

	// 卷集合装配（多卷，任务 3）：按 cfg.Volumes 逐卷 MkdirAll + storage.OpenRoot
	// （LAYOUT_VERSION 写入/校验）+ 卷容量 Pool + ACL 解析。缺省形态（YAML 只配
	// storage_root 未配 volumes）由 resolveDefaultVolumeRoot 裁决为 cfg.StorageRoot——
	// F1 门禁：Volumes[0].Root 占位（defaultStorageRoot）时不按它建根，防存储根静默漂移
	// 到 ./storage（单卷零回归红线）。失败（目录无法打开 / 布局版本不匹配）是致命装配
	// 错误：记 Error 并 panic 拒绝启动，绝不静默继续（否则文件服务会在错误的存储根上运行）。
	vs, err := assembleVolumes(cfg, log)
	if err != nil {
		log.Error("卷集合装配失败", "error", err)
		panic("卷集合装配失败: " + err.Error())
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
		totpNoncePool: newTotpNoncePool(),
		tracer:        opts.Tracer,
		auditRing:     auditRing,
		// per-AK 失败锁定表（U4）：恒装配（登录端点存在即需；上限 + 惰性清理见
		// loginFailTracker 注释）。
		loginFailTracker: newLoginFailTracker(),
		// 测试注入空 Ring 时的无认证调试兜底（一次性读取；生产走 cfg.AllowInsecureLoopback）。
		allowInsecureLoopback: opts.AllowInsecureLoopback,
		volSet:                vs,
	}
	// 装配多租户存储布局：默认卷租户缓存 + 全局配额池 + 预创建 anonymous 租户。
	// 默认卷租户缓存（h.tenants）在 globalRoot 赋值之后构造——它绑定该根；checksumStores/
	// uploadStores/quotaScopes 仍为懒创建（首次请求时建），见各 *For 辅助。
	// globalRoot 语义 = 默认卷根（F1 裁决；单卷形态 = cfg.StorageRoot，既有 tenantFor/handler
	// 默认卷语义零回归）；globalPool 仍是 owner 全局 max 兜底（cfg.MaxStorageBytes，跨卷合计）。
	h.globalRoot = vs.DefaultRoot()
	h.globalPool = quota.NewPool(cfg.MaxStorageBytes)
	// 默认卷租户：预建 meta 桶（per-tenant checksum / 凭据记录落点）。非默认卷由
	// pkg/volume/registry 按卷各持一个缓存（不预建 meta，见 Set.Tenant）。
	h.tenants = storage.NewTenantCache(h.globalRoot,
		storage.WithMetaBucket(), storage.WithLogger(h.logger))
	h.checksumStores = make(map[string]*checksum.ChecksumStore)
	h.uploadStores = make(map[string]*files.UploadStore)
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

	// 分享链接持久化（§10-③）：分享是服务级资源（token 全局唯一、跨租户可访问），
	// 落盘到 anonymous 租户 meta/share（<默认卷根>/anonymous/meta/share/）。
	// 启用后 Create/Consume/Revoke/过期清理同步原子写/删 <token>.json，重启恢复未过期链接。
	// 依赖 anonymous 租户已预建（上面 tenantFor(anonymousOwner) 检查通过 ⇒ meta 桶存在）。
	if tnt := h.tenantFor(anonymousOwner); tnt != nil && tnt.Root() != nil {
		if shareAbs, ok := tnt.Root().Abs("meta/share"); ok {
			h.shareStore.EnablePersist(shareAbs)
		}
	}

	// 凭据装配（凭据 store 化）：SproxySig 权威表 = Ring。
	//   - opts.CredentialRing 显式注入（测试 / cmd 装配）优先；
	//   - 否则从 opts.CredentialStore 载入快照重建；
	//   - **U3 零凭据启动**——store 为空不再生成首启 anonymous 凭据，系统以零凭据
	//     等待注册：register 公开端点是唯一用户入口，首个经回环注册的用户由
	//     AddRegistration 原子授 admin（DEC-F/D2）。空 store 时 bootstrapCredentials
	//     记启动日志提示「首次注册经回环，将成为 admin」（S2）。
	// storer 归一（typed-nil → nil）在 bootstrapCredentials 内完成（见 normalizeStorer）。
	h.bootstrapCredentials(opts)

	// 认证链装配（DEC-C）：宿主注入的 Authenticators 非 nil → replace 默认链（宿主
	// 全权掌控，需含 RingAuthenticator 则自行加入，R3-I2）。**显式注入空链（非 nil
	// 空切片）同样尊重**——空链 = 无任何 authenticator → 所有请求未认证（authMiddleware
	// 走 handleNoCredentials 兜底，不被默认链覆盖）；nil（未注入）→ 默认
	// [RingAuthenticator{credentialRing}]（R3-I1：4A 默认行为零回归）。
	if opts.Authenticators != nil {
		h.authenticators = opts.Authenticators
	} else {
		h.authenticators = []Authenticator{NewRingAuthenticator(h.credentialRing, h.noncePool, WithRingLogger(log))}
	}

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

	// 版本 GC 周期 goroutine（versioning.gc_interval > 0 时启动；0 = 关闭，零回归）。
	if cfg.Versioning.GCInterval > 0 {
		h.versionGCStop = make(chan struct{})
		h.versionGCWg.Add(1)
		go func() {
			defer h.versionGCWg.Done()
			h.versionGCLoop()
		}()
	}
	// 初始化 StorageManager 和 CloudDownloadManager。
	// P4：StorageManager 保留全局账本（sync/旧装配兼容）；启动扫描经 SetReconciler 按租户桶
	// 归集校准 per-tenant 配额 Scope（重启后 Scope 不回溯）。云任务配额走 cloud 桶子 Scope。
	// 多卷（任务 3）：StorageManager 扫描目录 = 默认卷根（vs.Default().RootDir——单一事实源，
	// 与 assembleVolumes 的 i==0 裁决共用同一装配产物，防两处裁决漂移；单卷形态 = cfg.StorageRoot，
	// 零回归）；reconcile 双目标——owner 全局 Scope（reconcileQuotaScopes）+ 默认卷容量池校准
	// （reconcileVolumePool）。多卷逐卷扫描校准框架见 reconcileVolumes（T4 与写路径一并接线）。
	sm := capacity.NewStorageManager(vs.Default().RootDir, cfg.MaxStorageBytes, nil, log.With("component", "storage"))
	defaultVolName := vs.Default().Name
	sm.SetReconciler(func(tenantBuckets map[string]map[string]int64) {
		// 单卷（含缺省形态）：StorageManager 已扫默认卷 → reconcileVolumePool 双校准（零回归）。
		if len(vs.All()) == 1 {
			h.reconcileVolumePool(defaultVolName, tenantBuckets)
			return
		}
		// 多卷（F2，AD-7 闭合）：逐卷扫描全部卷根双校准——默认卷归集已由本次 ScanAndRecalculate
		// 提供（tenantBuckets），其余卷在 reconcileVolumesFromDisk 内各自 capacity.ScanStorageDir。
		h.reconcileVolumesFromDisk()
	})
	_ = sm.ScanAndRecalculate() // 装配后重扫：校准 per-tenant Scope + 逐卷容量池（启动对账）
	cloudCfg := &cloud.CloudDownloadConfig{
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
	h.cloudMgr = cloud.NewCloudDownloadManager(vs.Default().RootDir, cloudStorageManager{m: sm}, h.tenantFor, h.checksumStoreFor, h.listTenantIDs, log.With("component", "cloud"), cloudCfg, func(owner string) *quota.Scope {
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
	// 卷 API（隧道内层裸注册：隧道加密即认证，与版本/share 同模式）
	localMux.HandleFunc("GET /api/volumes", h.listVolumesHandler)
	localMux.HandleFunc("POST /api/volumes/move", h.moveVolumeHandler)
	localMux.HandleFunc("GET /api/stats", h.statsHandler)
	localMux.HandleFunc("GET /api/config", h.configHandler)
	// 跨节点面只读运维视图（隧道内层：加密即认证，与 /api/config 同模式）。
	localMux.HandleFunc("GET /api/mesh/status", h.meshStatusHandler)
	localMux.HandleFunc("GET /api/mesh/acl", h.meshACLHandler)
	localMux.HandleFunc("PUT /api/config", h.updateConfigHandler)
	// 审计查看：隧道内层注册（无 authMiddleware——隧道加密即认证，与 /api/shares、
	// /api/stats 的 localMux 侧同模式）。auditHandler 只读 ring 回 JSON，自身不做
	// 签名校验。浏览器隧道模式下用户面操作必须隧道可达（仅注册主 mux 会 404）。
	localMux.HandleFunc("GET /api/audit", h.auditHandler)
	// 审计导出：隧道内层裸注册（无 authMiddleware——隧道加密即认证，与 /api/audit
	// 同模式）。导出供运维 CLI/日志 collector 消费，隧道模式下的运维面必须可达。
	localMux.HandleFunc("GET /api/audit/export", h.auditExportHandler)
	// 凭据管理（任务 5）：隧道内层裸注册（隧道加密即认证，与 audit/share 同模式）。
	// localMux 侧无 authMiddleware → 不经 SproxySig 验签，ActorFrom(ctx) 为空；本人
	// 判定依赖 actor 的端点（renew/sk 列表/删除/过期）在 localMux 侧按「未认证 404」
	// 处理，管理可见性面仍以主 mux（authMiddleware 保护）为准。
	// 公开注册端点 localMux 侧：浏览器隧道模式下凭据优先页需隧道可达（M1）。
	// 与主 mux 共用 registerPublic handler（同一 registerLimiter 实例，独立于
	// 文件传输限流）。
	localMux.Handle("POST /api/credentials/register", h.registerLimiter.Middleware(http.HandlerFunc(h.registerCredentialHandler)))
	// nonce 端点 localMux 侧：登录前置步骤须在隧道模式下可达（M1）；与主 mux 共用
	// 同一 totpLimiter 实例（login+nonce 共配额，任务⑨ login 挂同实例）。
	localMux.Handle("POST /api/credentials/nonce", h.totpLimiter.Middleware(http.HandlerFunc(h.nonceHandler)))
	// 登录端点 localMux 侧：TOTP 登录须在浏览器隧道模式下可达（M1）；与主 mux
	// 共用同一 loginLimiter 实例。
	localMux.Handle("POST /api/credentials/login", h.loginLimiter.Middleware(http.HandlerFunc(h.loginCredentialHandler)))
	localMux.HandleFunc("GET /api/credentials", h.akListHandler)
	localMux.HandleFunc("POST /api/credentials", h.akAddHandler)
	localMux.HandleFunc("DELETE /api/credentials/{ak}", h.akDeleteHandler)
	localMux.HandleFunc("POST /api/credentials/{ak}/renew", h.renewCredentialHandler)
	localMux.HandleFunc("GET /api/credentials/{ak}/sk", h.skListHandler)
	localMux.HandleFunc("DELETE /api/credentials/{ak}/sk/{skID}", h.skDeleteHandler)
	localMux.HandleFunc("POST /api/credentials/{ak}/sk/{skID}/expire", h.skExpireHandler)

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
		h.rateLimiter = rl
		apiHandler = rl.Middleware(apiHandler)
	}
	apiHandler = CORSMiddleware(cfg.CORS, log.With("component", "cors"))(apiHandler)
	// ★ MUST-FIX（任务②审查 Important）：隧道内层 requireRole 门禁收口——文件操作
	// 路由组在 localMux 侧同样过 requireRole(user)（DEC-C）。node 角色 AK 即便合法
	// 开隧道（外层 authMiddleware 放行），内层文件操作也 403，杜绝 node 借 /tunnel
	// 访问文件（匿名租户落桶）。
	//
	// ctx 透传语义（关键正确性点）：
	//   - 传统 POST /tunnel（tunnel.Handler dispatchLocal）：`http.NewRequestWithContext(
	//     r.Context(), ...)`——**内外层 ctx 共享**，外层认证注入的 Principal 天然透传
	//     内层，requireRole 用内层可读的 PrincipalFrom(ctx) 判定，node → 403、
	//     user/admin → 放行且落正确租户；
	//   - xfer 直连路径（tunnel_mux.handleStream）：`http.NewRequest(...)` 不携带外层
	//     ctx → PrincipalFrom(ctx)==nil。门禁要求「principal != nil 才收口」：xfer 面
	//     恒 nil → 不拦截（保持既有会话密钥身份语义），fail-open 由 xfer 握手本身
	//     （静态密钥 + Ed25519 pinning）闭合——node 账号不配置 xfer 凭据即无法建立
	//     xfer 会话。见 task-3-report.md「隧道内层门禁收口说明」。
	//   - 同理，凭据管理端点（renew/sk/ak 管理）在 localMux 侧保持裸注册（不包本
	//     门禁）：admin-only 判定依赖 ActorFrom（内层传统隧道路径为空 → 404），
	//     不被 requireRole 误伤；register 是公开端点（独立限频）。
	apiHandler = h.localMuxGate(apiHandler)

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

	// 文件操作路由组（DEC-C）：authMiddleware + requireRole(user) 门禁
	// （upload/download/delete/rename/list/stat/mkdir/rmdir/search/batch/chunk/
	// archive/versions/share…；Role∈{user,admin}）。
	srvMux.HandleFunc("POST /upload", h.fileRoute(h.upload))
	srvMux.HandleFunc("GET /download", h.fileRoute(h.download))
	srvMux.HandleFunc("POST /delete", h.fileRoute(h.delete))
	srvMux.HandleFunc("POST /rename", h.fileRoute(h.rename))
	srvMux.HandleFunc("GET /api/files", h.fileRoute(h.listFiles))
	srvMux.HandleFunc("HEAD /api/files/stat", h.fileRoute(h.stat))
	srvMux.HandleFunc("POST /upload/init", h.fileRoute(h.uploadInit))
	srvMux.HandleFunc("POST /upload/chunk", h.fileRoute(h.uploadChunk))
	srvMux.HandleFunc("GET /upload/status", h.fileRoute(h.uploadStatus))
	srvMux.HandleFunc("GET /upload/sessions", h.fileRoute(h.uploadSessions))
	srvMux.HandleFunc("POST /upload/complete", h.fileRoute(h.uploadComplete))
	srvMux.HandleFunc("GET /download/chunk", h.fileRoute(h.downloadChunk))
	srvMux.HandleFunc("POST /mkdir", h.fileRoute(h.mkdir))
	srvMux.HandleFunc("POST /rmdir", h.fileRoute(h.rmdir))
	srvMux.HandleFunc("GET /api/files/search", h.fileRoute(h.searchFiles))
	srvMux.HandleFunc("POST /api/batch/delete", h.fileRoute(h.batchDelete))
	srvMux.HandleFunc("POST /api/batch/rename", h.fileRoute(h.batchRename))
	srvMux.HandleFunc("POST /api/archive", h.fileRoute(h.archiveHandler))
	srvMux.HandleFunc("GET /api/archive-dir", h.fileRoute(h.archiveDirHandler))
	srvMux.HandleFunc("GET /api/versions", h.fileRoute(h.listVersionsHandler))
	srvMux.HandleFunc("POST /api/versions/restore", h.fileRoute(h.restoreVersionHandler))
	srvMux.HandleFunc("DELETE /api/versions", h.fileRoute(h.deleteVersionHandler))
	// 卷 API（主 mux：fileRoute = authMiddleware + requireRole(user)，per-owner 文件面）
	srvMux.HandleFunc("GET /api/volumes", h.fileRoute(h.listVolumesHandler))
	srvMux.HandleFunc("POST /api/volumes/move", h.fileRoute(h.moveVolumeHandler))
	srvMux.HandleFunc("GET /api/stats", h.authMiddleware(h.statsHandler))
	srvMux.HandleFunc("GET /api/config", h.authMiddleware(h.configHandler))
	srvMux.HandleFunc("GET /api/mesh/status", h.authMiddleware(h.meshStatusHandler))
	// /api/mesh/acl：owner 过滤**本身就是**边界（actor → owner），且不触碰文件系统，故不挂
	// fileRoute 的角色门禁（与 /api/mesh/status 同款；挂 fileRoute 反而会因不在文件组而 500）。
	srvMux.HandleFunc("GET /api/mesh/acl", h.authMiddleware(h.meshACLHandler))
	srvMux.HandleFunc("PUT /api/config", h.authMiddleware(h.updateConfigHandler))
	srvMux.HandleFunc("POST /api/share", h.fileRoute(h.createShareHandler))
	srvMux.HandleFunc("GET /s/{token}", h.accessShareHandler)

	// 分享管理 API（localMux：隧道内部使用）
	localMux.HandleFunc("POST /api/share", h.createShareHandler)
	localMux.HandleFunc("GET /api/shares", h.listSharesHandler)
	localMux.HandleFunc("DELETE /api/shares/{token}", h.revokeShareHandler)

	// 分享管理 API（主 mux：Bearer auth + requireRole(user) 门禁）
	srvMux.HandleFunc("GET /api/shares", h.fileRoute(h.listSharesHandler))
	srvMux.HandleFunc("DELETE /api/shares/{token}", h.fileRoute(h.revokeShareHandler))

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
		// NOTE(I29，属设计定位而非遗留项)：若要「经隧道做原始 TCP 中继」（链式中继/多跳），
		// 正确定位是 mux 层 raw-stream（复用 hub relay 模式），而非 http.Hijacker。见
		// .superpowers/sdd/i29-tunnel-hijack-value.md。

		// WebRTC 信令桥：SDP Offer/Answer/Candidate 存转 + 长轮询
		broker := h.signalBroker
		// S44：信令 POST 单独挂限流（独立实例，与文件传输隔离配额），防被攻破
		// 的已准入节点洪泛注入信令；GET poll 长轮询不挂（客户端高频轮询会误触发限流）。
		var signalPostRL *RateLimiter
		if cfg.RateLimit.Enabled {
			signalPostRL = NewRateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window, log.With("component", "signal_rate_limiter"))
			h.signalPostRL = signalPostRL
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
			// fail-closed：凭据 Ring 非空后无凭据请求 401），不注册 localMux
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

	// 审计查看 API：GET /api/audit 同时注册主 mux（authMiddleware，SproxySig/APIKey
	// 认证）与 localMux（隧道内层，隧道加密即认证——与 /api/shares、/api/stats 同
	// 模式）。审计是浏览器隧道模式下的用户面操作，隧道内层必须可达（用户在隧道
	// 模式下打开审计 tab 应能直接查看；仅注册主 mux 会让隧道模式 404）。
	srvMux.HandleFunc("GET /api/audit", h.authMiddleware(h.auditHandler))
	// 审计导出（主 mux 面）：authMiddleware 保护，与 /api/audit 同款（导出是敏感运维
	// 面，直连必须验签；localMux 面裸注册见上方注释）。
	srvMux.HandleFunc("GET /api/audit/export", h.authMiddleware(h.auditExportHandler))

	// 公开注册端点（4B DEC-F）：唯一用户入口，不挂 authMiddleware（主 mux +
	// localMux 双注册，仿 /healthz 层）——仅经独立限频 registerLimiter 收口。
	// register 激活前系统处于零凭据态，无凭据请求必须可直达（远程首注册仅经回环
	// 门禁拒绝，见 registerCredentialHandler）。
	//
	// registerLimiter 必须在 localMux 装配之前、localMux 侧复用同一限频器实例创建。
	h.registerLimiter = NewRateLimiter(5, time.Minute, log.With("component", "register_limiter"))
	registerPublic := h.registerLimiter.Middleware(http.HandlerFunc(h.registerCredentialHandler))
	srvMux.Handle("POST /api/credentials/register", registerPublic)

	// nonce 端点（task 8）：公开端点 + 独立限频 totpLimiter（login+nonce 共用，
	// D6/M1），不包 authMiddleware。totpLimiter 须在任何 localMux 装配之前创建（与
	// registerLimiter 同法，localMux 侧复用同一实例）。
	// 语义：服务器生成 16B 随机 nonce（sproxysig.NewNonce()，现有唯一实现）入
	// totpNoncePool（TTL 60s、单次使用、绑定来源 IP、池上限 4096、惰性清理，D5），
	// 供任务⑨ TOTP 登录作为 wrap-key context 的一次性子密钥来源。
	totpPerMin := 10
	if opts.TotpRateLimit > 0 {
		totpPerMin = opts.TotpRateLimit
	}
	h.totpLimiter = NewRateLimiter(totpPerMin, time.Minute, log.With("component", "totp_limiter"))
	srvMux.Handle("POST /api/credentials/nonce", h.totpLimiter.Middleware(http.HandlerFunc(h.nonceHandler)))

	// TOTP 登录端点（task 9）：公开端点 + 独立限频 loginLimiter（与 totpLimiter
	// 隔离配额——nonce 签发是预注册前的低频步骤，登录是高频爆破面，共用一个配额
	// 会让 nonce 签发挤掉登录防御预算），不包 authMiddleware。loginLimiter 须在
	// localMux 装配之前创建（与 registerLimiter/totpLimiter 同法，localMux 侧复用
	// 同一实例）。
	loginPerMin := 10
	if opts.LoginRateLimit > 0 {
		loginPerMin = opts.LoginRateLimit
	}
	h.loginLimiter = NewRateLimiter(loginPerMin, time.Minute, log.With("component", "login_limiter"))
	srvMux.Handle("POST /api/credentials/login", h.loginLimiter.Middleware(http.HandlerFunc(h.loginCredentialHandler)))

	// 凭据管理 API（主 mux：SproxySig auth）。全部走 authMiddleware 保护，
	// 与 audit/cloud/sync 同模式（本人 set 端点用 ActorFrom(ctx) 判定）。
	srvMux.HandleFunc("GET /api/credentials", h.authMiddleware(h.akListHandler))
	srvMux.HandleFunc("POST /api/credentials", h.authMiddleware(h.akAddHandler))
	srvMux.HandleFunc("DELETE /api/credentials/{ak}", h.authMiddleware(h.akDeleteHandler))
	srvMux.HandleFunc("POST /api/credentials/{ak}/renew", h.authMiddleware(h.renewCredentialHandler))
	srvMux.HandleFunc("GET /api/credentials/{ak}/sk", h.authMiddleware(h.skListHandler))
	srvMux.HandleFunc("DELETE /api/credentials/{ak}/sk/{skID}", h.authMiddleware(h.skDeleteHandler))
	srvMux.HandleFunc("POST /api/credentials/{ak}/sk/{skID}/expire", h.authMiddleware(h.skExpireHandler))

	srvMux.HandleFunc("GET /livez", h.livez)
	srvMux.HandleFunc("GET /readyz", h.readyz)
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

// isFileGroupedRoute 判定给定路由是否属于「文件操作组」（单一事实源，拒绝双份清单
// 漂移，F1/F2 收口）。文件操作组的成员 = 主 mux 面经 fileRoute 包装的路由全集：
//   - 精确匹配：upload/download/delete/rename、api/files*/mkdir/rmdir/batch*、
//     archive/versions/share/shares（share 列表 /api/shares 与撤销已含）；
//   - 前缀分支：/upload/{init,chunk,status,sessions,complete} 与 /download/chunk
//     （分块上传/下载，与 fileRoute 包裹的 chunk 组严格对齐）。
//
// 用途：
//   - fileRoute（主 mux 面）：path 命中组 → requireRole(user) 门禁；未命中 → 返回
//     errNotFileGrouped，调用方写 500 + Error 日志（fail-closed 防接线错误把文件类
//     新路由漏挂门禁——宁可显式故障，不为未列出的新文件路由静默放行）；
//   - localMuxGate（隧道内层面）：path 命中组 → principal 非 nil（传统 POST /tunnel
//     路径）时 requireRole(user) 收口、node → 403；principal nil（xfer 直连路径）
//     跳过（会话由握手密钥/pinning 闭合）；未命中 → 保持既有裸注册（隧道加密即
//     认证 / cloud/credentials/audit/stats/config/hub 等非文件面）。
//
// 新增文件类路由必须同步：既挂主 mux fileRoute，又在本函数补成员——两处同源，
// 漏其一即测试（TestRegister_TunnelInnerGate* / TestLocalMuxCoversAllTunnelRoutes）
// 暴露。
func isFileGroupedRoute(path string) bool {
	switch path {
	case "/upload", "/download", "/delete", "/rename",
		"/api/files", "/api/files/stat", "/api/files/search",
		"/mkdir", "/rmdir", "/api/batch/delete", "/api/batch/rename",
		"/api/archive", "/api/archive-dir",
		"/api/versions", "/api/versions/restore",
		"/api/volumes", "/api/volumes/move",
		"/api/share", "/api/shares",
		// 分块上传/下载（主 mux 面均挂 fileRoute——见 RegisterRoutes 装配处清单）；
		// 前缀含两个入口：/upload/{init,chunk,status,sessions,complete}。
		"/upload/init", "/upload/chunk", "/upload/status", "/upload/sessions", "/upload/complete",
		"/download/chunk":
		return true
	}
	// 动态参数路径组（Go 1.22 ServeMux {token} 通配——调用方传入的是实际 path，
	// 需按前缀判定）：/api/shares/{token}（撤销也属文件组）。精确列表 /api/shares
	// 已在上方案例命中；此处补带 token 子路径。
	if strings.HasPrefix(path, "/api/shares/") {
		return true
	}
	return false
}

// localMuxGate 包装隧道内层 localMux（含传统 POST /tunnel 与 xfer 直连两路径共用的
// apiHandler 链）：对「文件操作路由组」过 requireRole(PrincipalFrom(ctx), RoleUser)
// 门禁（任务② MUST-FIX 收口）。组成员 = isFileGroupedRoute（与主 mux fileRoute 同源）。
// 判定语义见 isFileGroupedRoute / RegisterRoutes 装配处大段注释。
func (h *Handlers) localMuxGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isFileGroupedRoute(r.URL.Path) {
			// 非文件组（cloud/credentials/register/audit/stats/config/hub/信令…）：
			// 保持既有裸注册（隧道加密即认证 / 免身份语义）。
			next.ServeHTTP(w, r)
			return
		}
		// 文件组：principal 非 nil（传统隧道路径外层已认证）才收口；nil（xfer
		// 直连，无外层身份）跳过——xfer 会话由握手密钥 / Ed25519 pinning 闭合身份。
		// 前提：HubXferKey 候选必须是 user/admin 凭据（node AK 不应进入 xfer 握手
		// 密钥候选集——若 node 凭据排序在前，xfer 面会对该 node 开放文件面），见
		// task-3-report.md「隧道内层门禁收口说明」。
		if p := PrincipalFrom(r.Context()); p != nil {
			if err := requireRole(p, string(accesskey.RoleUser)); err != nil {
				status := http.StatusForbidden
				if errors.Is(err, errUnauthorized) {
					status = http.StatusUnauthorized
				}
				http.Error(w, err.Error(), status)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// fileRoute 包装文件操作路由：authMiddleware 认证 + requireRole(user) 门禁（DEC-C）。
// 认证面插件化后，文件操作路由组要求 Role∈{user,admin}（minRole=user；R3-M4：空
// Role 归一 user 放行）。未认证且非回环直通（principal==nil）→ 401；角色不足 → 403。
//
// 组判定 = isFileGroupedRoute（单一事实源，与 localMuxGate 同源）：**未列出的路径
// 一律 500 + Error 日志**（fail-closed）——防止未来给文件类新路由只挂本包装却忘挂
// gate，或反之，静默绕过 requireRole。注意：fileRoute 只应包装文件组路由（主 mux
// 装配处全部如此）；非文件组路由继续用 authMiddleware（如 /api/cloud、/api/stats）。
func (h *Handlers) fileRoute(handler http.HandlerFunc) http.HandlerFunc {
	return h.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if !isFileGroupedRoute(r.URL.Path) {
			// fail-closed：本包装被错误用于非文件组路径（如未来把 cloud 路由误挂
			// fileRoute），显式拒绝并留痕，避免「看似受门禁实则裸放」的静默态。
			h.logger.Error("fileRoute 用于未列入文件组的路径（接线错误）", "method", r.Method, "path", r.URL.Path)
			http.Error(w, "internal: route not in file group", http.StatusInternalServerError)
			return
		}
		if err := requireRole(PrincipalFrom(r.Context()), string(accesskey.RoleUser)); err != nil {
			status := http.StatusForbidden
			if errors.Is(err, errUnauthorized) {
				status = http.StatusUnauthorized
			}
			http.Error(w, err.Error(), status)
			return
		}
		handler(w, r)
	})
}
