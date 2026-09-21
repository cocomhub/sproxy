// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// handlers.go 是装配层的**核心**：Handlers 结构体（持有全部注入依赖）、依赖注入 setter、
// 以及租户/主体/配额相关的装配助手（owner 归一与取用、tenantFor/tenantOf、checksumStoreFor、
// uploadStoreFor、配额 Scope 解析）。
//
// D3 拆分（2026-09-14）：本文件原为 1547 行单文件，按职责切为 5 个同包文件（**零 API 变更**，
// 纯代码搬迁，逐行比对已证）：handlers.go（本文件：结构体/注入/租户与配额）、
// routes.go（路由注册与路由级中间件）、credentials.go（凭据 Ring 装配与自省）、
// handlers_lifecycle.go（关闭与 hub 快照）、handlers_endpoints.go（健康/版本/UI 重定向与
// 上传残留清理）。
//
// 注意：`normalizeOwner` 必须留在本文件——`helper_impl_drift_test.go` 的
// TestNormalizeOwner_DelegatesToStorage 按**文件路径**断言它委托 storage.NormalizeOwner。

package server

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
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
	auditLogger *slog.Logger
	// meshRuntimeInfo 是跨节点面/角色的**运行态**提供者（装配层注入，见 mesh_status.go）。
	// nil = 未注入（视图只反映配置态）。
	meshRuntimeInfo func() MeshRuntimeInfo

	// auditRing 是有界内存环形审计缓冲（cfg.Audit.BufferSize，默认 2048；0=关闭）。
	// RegisterRoutes 按 cfg 装配（BufferSize>0 时创建）；RecordAudit 在 TS 填充后
	// 挂钩 Add，所有审计录入点自动进 ring。nil = 关闭（GET /api/audit 返回空表）。
	auditRing      *AuditRing
	cloudMgr       *cloud.CloudDownloadManager
	syncMgr        *syncmgr.Manager // 文件同步任务管理器（nil = 未配置 sync，相关路由返回 400）
	storageMgr     *capacity.StorageManager
	uploadingFiles sync.Map       // map[string]string — filename → uploadID，追踪正在上传的文件名
	uploadingStop  chan struct{}  // 关闭后通知 uploadingFiles 定期清理 goroutine 退出
	uploadingWg    sync.WaitGroup // 等待 cleanupUploadingFilesLoop 退出
	// versionGCStop / versionGCWg 是版本 GC 周期 goroutine 的停止信号与等待组
	// （仅 versioning.gc_interval > 0 时挂载；与 uploading 清理 goroutine 同构）。
	versionGCStop chan struct{}
	versionGCWg   sync.WaitGroup
	// mirrorStop / mirrorWg 是卷镜像周期 goroutine 的停止信号与等待组
	// （仅 cfg.MirrorInterval > 0 时挂载；与 versionGC 同构）。
	mirrorStop chan struct{}
	mirrorWg   sync.WaitGroup
	// rotationStop / rotationWg 是凭据自动轮换周期 goroutine 的停止信号与等待组
	// （仅 credentials.rotation.interval > 0 时挂载；与 versionGC 同构）。
	rotationStop chan struct{}
	rotationWg   sync.WaitGroup
	closeOnce    sync.Once            // 防止 Close() 重复关闭 channel
	noncePool    *sproxysig.NoncePool // SproxySig nonce 防重放池
	// rateLimiter 是隧道内层 API handler 的全局限流器（RegisterRoutes 在
	// cfg.RateLimit.Enabled 时创建并挂到 apiHandler）。PUT /api/config 经 configMu
	// 保护调用 UpdateConfig 热更新（含 enabled/limit/window），无需重建 handler 链
	// （xfer LocalHandler 已持有构造期引用）。nil = 启动未启用限流。
	rateLimiter *RateLimiter
	// signalPostRL 是信令 POST 专用限流器（独立实例，与文件传输隔离配额）。
	// 热更新时与 rateLimiter 一起更新；nil = 启动未启用或未装配 RouteTable。
	signalPostRL *RateLimiter
	// tracer 是服务端请求路径的可选 telemetry.Tracer（RegisterRoutesOpts.Tracer
	// 注入；nil = 不接通，保持自生成 id 的既有行为）。非 nil 时 requestLogMiddleware
	// 为该请求建立真实 span（trace/span id 来自 tracer），ctx 携带 core.SpanContext，
	// WithContextHandler 自动把 trace_id/span_id 注入日志。nil = 默认 no-op。
	tracer telemetry.Tracer

	// 多租户存储布局装配（任务 4，供 P2/P3 各 handler 迁移复用）。
	// globalRoot 是 OpenRoot 后的全局存储根（含 LAYOUT_VERSION 校验）；
	// globalPool 是全局配额池（cfg.MaxStorageBytes 兜底）。租户/checksum/配额均懒创建并缓存，
	// tenantMu 串行化懒创建（无竞态）；P2 各 handler 逐个切换到 tenantOf/checksumStoreFor/quotaFor。
	globalRoot     *storage.Root                      // 全局存储根（OpenRoot + LAYOUT_VERSION）
	globalPool     *quota.Pool                        // 全局配额池（cfg.MaxStorageBytes 兜底）
	tenants        *storage.TenantCache               // 按 owner 缓存租户（含 anonymous；懒创建）
	checksumStores map[string]*checksum.ChecksumStore // 按 owner 缓存 per-tenant checksum 存储
	uploadStores   map[string]*files.UploadStore      // 按 owner 缓存 per-tenant 分块上传存储（懒创建）
	dedupStores    map[string]*files.DedupStore       // 按 owner 缓存 per-tenant 去重台账（懒创建）
	quotaScopes    map[string]*quota.Scope            // 按 owner 缓存配额 Scope（globalPool.Scope 懒创建）
	quotaBuckets   map[string]map[string]*quota.Scope // 按 owner 缓存功能桶配额子 Scope（user/cloud/archive/chunk/version）
	// archiveUsage 按 owner 登记已确认占用的归档文件（archive 桶），供删除时释放 Scope
	// （P5 审查重要 2：不依赖周期扫描自愈）。tenantMu 保护。
	archiveUsage map[string]map[string]int64
	tenantMu     sync.Mutex // 串行化 checksumStores/uploadStores/quotaScopes/quotaBuckets/archiveUsage 懒创建
	// volSet 是装配后的卷集合（RegisterRoutes 装配；nil = 未装配卷功能的旧装配路径，如
	// 测试手工构造的 Handlers）。默认卷语义：globalRoot 字段 = 默认卷根、tenantFor 走默认卷。
	// 写路径本任务仍只走默认卷（T4 起卷感知），volSet 供多卷 reconcile 与后续卷路由消费。
	volSet *registry.Set

	// userVolumes 是用户自有卷 store（U3：per-owner volume meta 持久化；nil = 未装配，
	// 相关 /api/volumes/user 路由返回 400）。
	userVolumes *UserVolumeStore
	// credentialRing 是 SproxySig 凭据权威表（AK→多 SK 条目，凭据 store 化后取代
	// cfg.AccessKeys）。RegisterRoutes 装配：opts.CredentialRing 显式注入（测试/
	// xfer 集成）优先；否则从 opts.CredentialStore 载入（见 bootstrapCredentials）。
	// **U3 零凭据启动**——载入后仍空不再生成 anonymous，系统以零凭据等待注册：
	// register 公开端点是唯一用户入口，首个经回环注册的用户原子授 admin。
	// authMiddleware 只查本 ring、无 yaml 回退。
	// credentialStore 是 credentialRing 关联的持久化 store（凭据变更后 Save；
	// nil = 不持久化，纯内存场景）。持接口类型，可注入外部 storer 实现。
	credentialRing  *accesskey.Ring
	credentialStore accesskey.CredentialStorer
	// authenticators 是认证面插件化链（DEC-C）：authMiddleware 遍历链，任一成功 →
	// Principal 入 ctx 并放行。RegisterRoutes 装配：opts.Authenticators 显式注入
	// （非 nil → replace 默认链，宿主全权掌控）优先；nil → 默认
	// [RingAuthenticator{credentialRing}]。api_keys Bearer 是链前独立检查，不入链。
	authenticators []Authenticator
	// allowInsecureLoopback 是无认证兜底开关（读取优先级：opts 注入 > cfg 配置）。
	// 仅调试语义：ring 为空时放行 loopback 来源（见 handleNoCredentials）。
	allowInsecureLoopback bool
	// registerLimiter 是公开注册端点的独立限频器（复用 pkg/server.RateLimiter，
	// per-IP 令牌桶 + 全局滑动窗口，5/min 封顶，D6/M1）。注册是唯一用户入口（U3）
	// 且不经 authMiddleware——蓄意攻击者可任意 IP 洪泛，故逐 IP 限频 + 全局窗口
	// 兜底。与文件传输/信令限流（rateLimiter/signalPostRL）隔离配额。nil = 启动未装配。
	registerLimiter *RateLimiter
	// totpNoncePool 是 TOTP 登录 nonce 池（POST /api/credentials/nonce 签发，
	// 任务⑨ login 消费）：map[nonce]→{expiresAt, ip}，TTL 60s、单次使用（任何消费
	// 即删）、绑定来源 IP、池上限 maxTotpNoncePool、插入/消费时惰性清理过期项（D5）。
	//
	// **不复用 sproxysig.NoncePool（M10）**：那是按 (ak, nonce) 去重的签名防重放池，
	// 语义不同——登录 nonce 需单次消费（任一登录尝试即删，防同 nonce 爆破）、绑定
	// 来源 IP、无 AK 前缀、池上限 4096。
	totpNoncePool *totpNoncePool
	// totpLimiter 是 TOTP 登录 + nonce 端点共用的独立限频器（10/min，D6/M1）。
	// 公开端点的暴力破解防线，与 registerLimiter 隔离配额。
	totpLimiter *RateLimiter
	// loginFailTracker 是 per-AK TOTP 登录失败锁定表（U4）：map[ak]→
	// {failCount, lockedUntil} + mutex。连续失败达 cfg.Registration.LoginFailLimit
	// → 锁定 LoginFailWindow（锁定期内该 AK 登录一律 401，含正确动态码，不随 IP
	// 变化失效）；登录成功清零。map 上限 1024 + 惰性清理（R2-N1）——插入时若已达
	// 上限，先剪掉 lockedUntil 已过期的条目，仍满则按 map 迭代序淘汰任意一条
	// （**无严格 LRU 语义**，只钳制无界增长），防 map 膨胀。空 AK 键跳过（不登记）。
	// 登录失败计数语义（R2-N2）见 register_handler.go recordLoginFailure 注释。
	loginFailTracker *loginFailTracker
	// loginLimiter 是 POST /api/credentials/login 的独立限频器（10/min；与
	// totpLimiter 语义同级，独立实例避免 nonce 签发与登录消费互相挤压配额，
	// D6/M1）。公开端点，无 authMiddleware。
	loginLimiter *RateLimiter
	// totpPending 是 TOTP 注册 pending 表（两段式提交）：register 只生成 pending
	// （AK + TOTP secret + owner + 过期时间），**不写 ring / 不落盘**；客户端用正确
	// TOTP 动态码登录成功才提交（AddRegistration + persist）。
	//   - 首 admin 单槽：无 admin 时 pending 表只允许一条（防并发 pending 抢 admin）；
	//   - owner 幂等：同 owner 已有活跃 pending → 409（不产生第二个候选）；
	//   - 绑定失败自动回收：TTL 过期 / 登录失败达阈值 → 删 pending（AK 可复用）；
	//   - 纯内存态（重启即清，与 nonce 池同生命周期），TTL 默认 10 分钟。
	totpPending *totpPendingTable

	// filesSvc 是文件服务域实例（pkg/files）。经 fileService() 懒装配：文件服务域只
	// 依赖 h 的窄能力（见 filesRuntime），构造时机不影响语义，而 *Handlers 有多条构造
	// 路径（RegisterRoutes 正式装配、测试手工构造），懒装配让两条路径都无需改动。
	filesSvc     *files.Service
	filesOnce    sync.Once
	eventsBus    *EventBus
	eventBusOnce sync.Once

	// bwBuckets 是带宽限速 per-owner 令牌桶缓存（rate_limit.bandwidth 启用时懒建）。
	bwBuckets sync.Map

	// bwCoordOnce / bwCoord 是带宽跨实例协调器（coord_backend=file）单例缓存。
	bwCoordOnce sync.Once
	bwCoord     *byteFileCoordinator
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

// LocalHandler 返回隧道内层本地文件 API handler（localMux + 中间件链，
// 不含外层帧解密/密钥检查）。
//
// 供 xfer listener（阶段 5 工作项 1）直接路由解密后的隧道请求：xfer 隧道
// handleStream 已把请求体解密为明文，无需再经 `POST /tunnel` 的外层帧解密
// （NewLocalHandler 期望请求 ctx 带派生密钥且 body 为帧协议——xfer 请求两者皆无，
// 直接使用会 401 unauthorized）。两者互补：`POST /tunnel` 路由用 `h.tunnelHandler`
// 字段做外层帧解密，xfer 隧道用本方法拿明文入站 handler。
func (h *Handlers) LocalHandler() http.Handler {
	return h.localHandler
}

// fileService 返回文件服务域实例（pkg/files），首次调用时按当前装配状态构造并缓存。
//
// 装配方式是 **Option 构造**（见 pkg/files/options.go）：唯一必需项是租户解析（filesRuntime），
// 其余能力经 With* 注入同一个 filesRuntime（它实现领域声明的全部能力接口）。
// nil 语义（未装配卷集合/容量）由 filesRuntime 内部判 nil 表达，不再有 typed-nil 守卫。
//
// **形状分类**（以接口方法表达，不再需要区分取用函数/快照值）：
//   - 日志器是**取用函数**（WithLogger）：h.logger 会在日志配置热更新时就地替换，
//     快照会写旧 handler；
//   - 其余能力都是接口方法：每次调用读 h 的实时字段/懒建缓存（如 cfgPtr.Load()、
//     volSet、quotaScopeFor），故配置与缓存的变化对领域包可见；
//   - 计量条件注入：h.metrics 为 nil 时不加 WithMetrics（避免 typed-nil 接口）。
func (h *Handlers) fileService() *files.Service {
	h.filesOnce.Do(func() {
		rt := filesRuntime{h: h}
		opts := []files.Option{
			files.WithLogger(func() *slog.Logger { return h.logger }),
			files.WithActor(rt),
			files.WithVolumes(rt),
			files.WithQuota(rt),
			files.WithChecksumLedger(rt),
			files.WithDownloadPaths(rt),
			files.WithFileLocks(rt),
			files.WithChunkedUploads(rt),
			files.WithVersioning(rt),
			files.WithDedup(rt),
			files.WithAudit(rt),
			files.WithEventSink(rt),
			files.WithUploadBodyLimit(func() int64 { return int64(h.cfgPtr.Load().MaxUploadBytes) }),
			files.WithBandwidthLimiter(rt),
		}
		if h.metrics != nil {
			opts = append(opts, files.WithMetrics(h.metrics))
		}
		// 唯一必需项（TenantResolver）由 *storage.TenantCache 直接满足——它绑定默认卷根，
		// 与 pkg/server 的 tenantFor 同一实例（单一租户缓存），无需适配类型。
		svc, err := files.New(h.tenants, opts...)
		if err != nil {
			// 唯一必需项（TenantResolver）由 rt 提供，不可达；保留 fail-fast 以防未来改动。
			panic("files.New 装配失败: " + err.Error())
		}
		h.filesSvc = svc
	})
	return h.filesSvc
}

// anonymousOwner 是未认证请求的默认租户名（结构与其他租户完全同构）。
// 单源在 pkg/storage：租户名是存储布局契约（<root>/<owner>/…），各包必须同值。
const anonymousOwner = storage.AnonymousOwner

// normalizeOwner 把空 owner 归一为 anonymous 租户名（未认证请求的默认租户）。
func normalizeOwner(owner string) string {
	return storage.NormalizeOwner(owner)
}

// ownerFromRequest 返回请求 ctx 中的操作主体（未认证返回 ""）。
func ownerFromRequest(r *http.Request) string {
	return ActorFrom(r.Context())
}

// tenantFor 返回 owner 的租户（空 owner → anonymous）。
//
// 懒创建 + 缓存 + 失败关闭的执行体单源在 pkg/storage.TenantCache（创建骨架为
// storage.OpenTenant）：本方法只是一行转发，装配层与 pkg/volume/registry 共用同一份
// 实现与同一套 fail-closed 语义（不再各写一份）。
//
// 租户磁盘布局 = <存储根>/<owner>/（默认卷根 = globalRoot）；预建 meta 桶（per-tenant
// checksum / 凭据记录落点，见装配处的 WithMetaBucket）。未装配缓存（或已关闭）时返回 nil
// ——调用方按 400 处理，**绝不回落全局根**；非法 owner 同样 fail-closed。
func (h *Handlers) tenantFor(owner string) *storage.Tenant {
	return h.tenants.TenantFor(normalizeOwner(owner))
}

// tenantOf 返回请求者 owner 的租户（owner 空 → anonymous 租户）。构造失败返回 nil
// （调用方按 400 处理，绝不回落全局根）。
func (h *Handlers) tenantOf(r *http.Request) *storage.Tenant {
	return h.tenantFor(ownerFromRequest(r))
}

// listTenantIDs 返回存储根下全部租户名（磁盘扫描，按名排序）。
// 供 CloudDownloadManager 恢复扫描使用：进程重启后内存租户缓存只有已访问的
// 租户（anonymous 预创建），仅靠缓存会漏掉已落盘但尚未访问的租户（如 alice 的云任务）。
// 扫描实现（含 .__ / __ 内部目录过滤）在 pkg/storage.ListOwners。
func (h *Handlers) listTenantIDs() []string {
	return storage.ListOwners(h.globalRoot)
}

// checksumStoreFor 返回 owner 的 per-tenant checksum 存储（懒创建，缓存到 map）。
// storePath = <tenant meta>/checksums.json；获取不到租户（非法 owner / 根不可用）返回 nil。
// P5 后不再有全局 checksum store——所有读写侧均经本方法取 per-tenant 实例。
func (h *Handlers) checksumStoreFor(owner string) *checksum.ChecksumStore {
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
	cs := checksum.NewChecksumStore(filepath.Join(metaAbs, "checksums.json"), h.logger)
	h.checksumStores[owner] = cs
	return cs
}

// uploadStoreFor 返回 owner 的 per-tenant 分块上传存储（懒创建，缓存到 map）。
// 存储根 = 租户根下 chunk 桶（<root>/<owner>/chunk/，经 Tenant.Root().Abs("chunk")
// 派生绝对路径）。每租户独立 UploadStore 实例 → 会话天然物理隔离（会话目录
// <root>/<owner>/chunk/<uploadID>/），upload_id 无需 owner 前缀；跨租户同裸 id 互不可见。
// 获取不到租户（非法 owner / 根不可用）或创建失败返回 nil（调用方按 500/404 处理）。
func (h *Handlers) uploadStoreFor(owner string) *files.UploadStore {
	owner = normalizeOwner(owner)
	// 先取租户（内部锁 tenantMu，懒创建租户根）。
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return nil
	}
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if h.uploadStores == nil {
		h.uploadStores = make(map[string]*files.UploadStore)
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
	// 多卷（AD-5）：会话可跨卷定卷，UploadStore 需按 session.Volume 解析目标卷租户根
	// （temp 文件删除/恢复/过期清理）。**卷根注册必须先于 NewUploadStore 的 recoverSessions**
	// ——recover 按 session.Volume 解析在途 temp 文件，非默认卷会话若未预注册会把 temp 解析
	// 到默认卷 → 打开失败清空 bitmap → 断点续传退化为整文件重传（T6a 修复轮发现-1）。
	// Root.Abs 只推导不创建目录（无副作用——目标卷租户目录仍由写路径首次使用时懒建）。
	us, err := files.NewUploadStore(chunkAbs, sessionTTL, h.logger.With("component", "upload_store", "tenant", owner),
		h.uploadVolumeRootsFor(owner))
	if err != nil {
		h.logger.Error("创建 per-tenant UploadStore 失败", "tenant", owner, "error", err)
		return nil
	}
	// P5：quota 未装配（globalPool nil）时，分块上传走 storageMgr 回退预留，
	// 需把 storageMgr 注入 store 供会话删除/过期释放（scope 预留路径无需）。
	if h.storageMgr != nil {
		us.SetStorageMgr(filesStorageManager{h.storageMgr})
	}
	h.uploadStores[owner] = us
	return us
}

// dedupStoreFor 返回 owner 的 per-tenant 去重台账（懒创建，缓存到 map，与 checksumStoreFor 同构）。
// 台账路径 = <tenant meta>/dedup.json；dedup 未启用或获取不到租户返回 nil。
// 缓存复用同一实例：台账内存态增量保存回磁盘，避免高频上传每次磁盘 Load（#424 残余优化）。
func (h *Handlers) dedupStoreFor(owner string) *files.DedupStore {
	if !h.cfgPtr.Load().Dedup.Enabled {
		return nil
	}
	owner = normalizeOwner(owner)
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return nil
	}
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if h.dedupStores == nil {
		h.dedupStores = make(map[string]*files.DedupStore)
	}
	if ds, ok := h.dedupStores[owner]; ok {
		return ds
	}
	metaAbs, ok := tnt.Root().Abs("meta")
	if !ok {
		h.logger.Warn("派生租户 meta 路径失败", "owner", owner)
		return nil
	}
	ds := files.NewDedupStore(filepath.Join(metaAbs, "dedup.json"), h.logger)
	h.dedupStores[owner] = ds
	return ds
}

// uploadVolumeRootsFor 返回 owner 在各**非默认**卷的租户根绝对路径映射（供 UploadStore
// recover/DeleteSession/cleanupExpired 按 session.Volume 解析目标卷 temp 路径）。
// 默认卷（""）回落 UploadStore.baseDir 父目录，无需注册。Root.Abs 只推导不创建目录。
// 调用方须已持 h.tenantMu（uploadStoreFor 内调用）。
func (h *Handlers) uploadVolumeRootsFor(owner string) map[string]string {
	roots := map[string]string{}
	if h.volSet == nil {
		return roots
	}
	for _, v := range h.volSet.All() {
		if v.Name == h.volSet.Default().Name {
			continue
		}
		if rt := h.volSet.Root(v.Name); rt != nil {
			if abs, ok := rt.Abs(owner); ok {
				roots[v.Name] = abs
			}
		}
	}
	return roots
}

// quotaBucketNames 是参与配额归集的功能桶名（对应租户根下的物理桶；meta 桶的配额
// 参与归集与否由任务 3 的扫描开启决定，此处先装配其子 Scope 供 reconciliation 使用）。
var quotaBucketNames = []string{"user", "cloud", "archive", "chunk", "version", "meta"}

// ensureTenantQuotaLocked 确保 owner 的租户配额 Scope、功能桶子 Scope 与 bucket_limits
// 路径子 Scope 已创建（调用方须持 tenantMu）。首次访问按 owner 在 globalPool 下挂载
// /tenant/<owner> Scope（上限 = cfg.OwnerQuotaFor(owner)），并预创建
// user/cloud/archive/chunk/version/meta 功能桶子 Scope（上限 0 = 不限制，租户上限由父
// Scope 单一执行），随后按 cfg.BucketLimits 对每个相对路径建精确路径子 Scope
// （scope.Mount(path, limit)，键即完整逻辑路径，供 quotaBucketFor 精确路径命中复用）。
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
		if cfg.BucketLimits != nil {
			bucketLimits = make(map[string]int64, len(cfg.BucketLimits))
			for k, v := range cfg.BucketLimits {
				bucketLimits[k] = int64(v)
			}
		}
	}
	s := h.globalPool.Scope("/tenant/"+owner, quotaBytes)
	buckets := make(map[string]*quota.Scope, len(quotaBucketNames)+len(bucketLimits))
	for _, b := range quotaBucketNames {
		buckets[b] = s.Mount(b, 0)
	}
	for path, limit := range bucketLimits {
		// BucketLimits 分层装配：键如 "user/videos/hd"，拆段逐级挂到功能桶（user）children
		// 之下（http route 式嵌套 Scope）。子目录 Scope 沿父链聚合到 user 桶 → 租户 → 全局，
		// 对子 Scope 记一笔账即自动逐级检查所有层级上限。quotaBuckets map 保留配置键 → Scope
		// 引用（供配置校验/测试/旧调用），写路径解析走 quotaScopeFor（沿段树最长前缀）。
		segs := strings.Split(filepath.ToSlash(path), "/")
		if len(segs) == 0 {
			continue
		}
		rootSc, ok := buckets[segs[0]]
		if !ok {
			// 配置校验已强制首段为功能桶（config.go），此处防御跳过非功能桶首段。
			continue
		}
		buckets[path] = rootSc.EnsureScope(segs[1:], limit)
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
// 语义：沿功能桶段树 Resolve(bucket)（最长前缀命中）；bucket 为功能桶名时返回功能桶
// 根（无子目录键时回落到它）。globalPool 未装配时返回 nil；任意非白名单/未配置路径
// 不再建任意子 Scope——沿段树未命中即回落功能桶根（不再返回 nil，除非功能桶根本身
// 缺失/globalPool 未装配）。子 Scope 操作沿父链聚合到功能桶 → 租户 Scope → globalPool。
func (h *Handlers) quotaBucketFor(owner, bucket string) *quota.Scope {
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	_, buckets := h.ensureTenantQuotaLocked(owner)
	if buckets == nil {
		return nil
	}
	segs := strings.Split(filepath.ToSlash(bucket), "/")
	if len(segs) == 0 || segs[0] == "" {
		return nil
	}
	rootSc, ok := buckets[segs[0]]
	if !ok {
		return nil // 非功能桶首段 → 无子 Scope
	}
	if len(segs) == 1 {
		return rootSc
	}
	return rootSc.Resolve(segs[1:])
}

// quotaScopeFor 按文件实际相对路径（rel，含功能桶前缀，如 "user/dir/f.txt"）解析最长前缀
// 配额子 Scope（http route 式）：rel 首段 = 功能桶根，沿其 children 段树逐级下探；最长前缀
// 命中（如 rel="user/videos/hd/a.mkv" 且装配了 "user/videos/hd" → 返回该子 Scope）；
// 无子目录键时回落功能桶根。对返回值 TryReserve 即沿父链自动逐级检查所有层级上限——
// 所有写路径（上传/分块/版本/删除/rmdir/sync pull）统一走本函数，子目录配额即封顶。
func (h *Handlers) quotaScopeFor(owner, rel string) *quota.Scope {
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	_, buckets := h.ensureTenantQuotaLocked(owner)
	if buckets == nil {
		return nil
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	if len(segs) == 0 || segs[0] == "" {
		return nil
	}
	rootSc, ok := buckets[segs[0]]
	if !ok {
		return nil // 非功能桶首段 → 无子 Scope
	}
	if len(segs) == 1 {
		return rootSc // 功能桶根内的文件（user/a.txt）
	}
	return rootSc.Resolve(segs[1:])
}

// bwBucketFor 返回 owner 的带宽令牌桶（懒建缓存；限速关闭/无速率时 nil = 不限速）。
// CoordBackend=file 时桶装配跨实例协调器（byteFileCoordinator，按 owner 共享字节配额）。
func (h *Handlers) bwBucketFor(owner string, bps, burst int64) *files.TokenBucket {
	if bps <= 0 {
		return nil
	}
	if v, ok := h.bwBuckets.Load(owner); ok {
		return v.(*files.TokenBucket) //nolint:errcheck // 类型断言安全：只存 *TokenBucket
	}
	b := files.NewTokenBucket(bps, burst)
	// file 协调后端：装配跨实例字节配额协调器（coord_backend=file，按 owner key）。
	if cfg := h.cfgPtr.Load(); cfg.RateLimit.Bandwidth.CoordBackend == "file" {
		if coord := h.bwCoordinatorFor(cfg); coord != nil {
			b.SetCoordinator(owner, coord)
		}
	}
	actual, _ := h.bwBuckets.LoadOrStore(owner, b)
	return actual.(*files.TokenBucket) //nolint:errcheck
}

// bwCoordinatorFor 构造带宽跨实例协调器（coord_backend=file）：按 owner 字节预算的
// 文件原子计数后端。协调器单例缓存（bwCoordOnce）；未启用/后端 local → nil。
func (h *Handlers) bwCoordinatorFor(cfg *Config) *byteFileCoordinator {
	h.bwCoordOnce.Do(func() {
		dir := cfg.StorageRoot
		if dir == "" {
			h.bwCoord = nil
			return
		}
		h.bwCoord = newByteFileCoordinator(cfg.RateLimit.Bandwidth.PerOwnerBPS, time.Second, dir, h.logger)
	})
	return h.bwCoord
}
