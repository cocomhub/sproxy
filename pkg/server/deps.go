// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// deps.go 是 Handlers 的**分域依赖**（server 治理 S4）：100+ 扁平字段按
// 「将来要搬去的切片」分组，Handlers 以**值嵌入**组合。
//
// 为什么是值嵌入而不是具名子结构体：Go 的字段提升（promotion）让 `h.volSet` 继续可用，
// 现有 580 个 *Handlers 方法与全部 `Handlers{...}` 字面量**零改动**；同时 Handlers
// 从 100+ 字段收敛为 12 个分组，后续切片搬走时直接把这个 struct 变成该切片的 Deps
// （字段名即注入点）。
//
// 为什么是值嵌入而不是指针：零值 Handlers 仍可安全读取任意分组字段（指针嵌入会 nil panic）；
// 分组本身无状态，复制成本可忽略（Handlers 始终以 *Handlers 传递）。
package server

import (
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/authn"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/leader"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/server/ratelimit"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// coreDeps 是装配核心（配置/日志/指标/生命周期）域依赖。
type coreDeps struct {
	cfgPtr *atomic.Pointer[Config]
	// skipFileMetaAssembly 是测试隔离开关（AGENTS.md 7b：测试注入走结构体内部私有
	// 变量）：存量语义测试用 newAssemblyTestHandlers 置 true 跳过 filesMetaPolicy 装配
	// （meta 不生成，配额/统计/账本断言不被 sidecar 字节干扰）。生产恒 false——
	// meta 恒生成（用户裁定：只关校验不关 meta）。
	skipFileMetaAssembly bool
	version              string
	buildAt              string
	// removeMovedSource 是 move 删源实现（nil = 默认 storage.Root.Remove）。**测试注入
	// opt（2026-10-07 用户裁定：禁止测试依赖全局函数/并发修改包级 seam）**：RegisterRoutes
	// 构造时经 opts.RemoveMovedSource 注入实例字段，运行期不可变（无包级 seam 并发替换）。
	removeMovedSource func(root *storage.Root, rel string) error
	logger            *slog.Logger
	metrics           *Metrics
	closeOnce         sync.Once // 防止 Close() 重复关闭 channel
	closeRootsOnce    sync.Once // 防止并发 Close（优雅停服 defer + 信号路径）重复关存储根（volSet/globalRoot 置 nil 竞态）
	// tracer 是服务端请求路径的可选 telemetry.Tracer（RegisterRoutesOpts.Tracer
	// 注入；nil = 不接通，保持自生成 id 的既有行为）。非 nil 时 requestLogMiddleware
	// 为该请求建立真实 span（trace/span id 来自 tracer），ctx 携带 core.SpanContext，
	// WithContextHandler 自动把 trace_id/span_id 注入日志。nil = 默认 no-op。
	tracer telemetry.Tracer
	// mutationHandler 是本地卷 Root 变异事件的处理函数（索引 + 卷状态缓存）。
	// **为什么用原子指针**：观察者必须在**任何租户子根创建之前**装到卷根上（否则已缓存的
	// 租户根不会继承——启动期 initStorageLayout 会先建 anonymous 租户），而真正的处理函数要
	// 等 files.Service 装配完成才能构造 ⇒ 先装「转发器」，装配完成后整体换指针。
	mutationHandler atomic.Pointer[func(storage.Mutation)]
}

// httpDeps 是HTTP 面（mux handler/限流）域依赖。
type httpDeps struct {
	tunnelHandler http.Handler
	// localHandler 是隧道内层本地文件 API handler（localMux + 中间件链，不含外层
	// 帧解密/密钥检查）。供 xfer listener（阶段 5 工作项 1）直接路由解密后的隧道
	// 请求；与 tunnelHandler（传统 POST /tunnel 外层帧解密器）互补。
	localHandler http.Handler
	handler      http.Handler
	// rateLimiter 是隧道内层 API handler 的全局限流器（RegisterRoutes 在
	// cfg.RateLimit.Enabled 时创建并挂到 apiHandler）。PUT /api/config 经 configMu
	// 保护调用 UpdateConfig 热更新（含 enabled/limit/window），无需重建 handler 链
	// （xfer LocalHandler 已持有构造期引用）。nil = 启动未启用限流。
	rateLimiter *ratelimit.RateLimiter
	// signalPostRL 是信令 POST 专用限流器（独立实例，与文件传输隔离配额）。
	// 热更新时与 rateLimiter 一起更新；nil = 启动未启用或未装配 RouteTable。
	signalPostRL *ratelimit.RateLimiter
}

// authDeps 是认证与凭据（Ring/TOTP/登录限制）域依赖。
type authDeps struct {
	noncePool *sproxysig.NoncePool // SproxySig nonce 防重放池
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
	// 外部认证（OIDC/LDAP）子认证器经 opts.ExternalAuthHandlers 追加到链尾。
	authenticators []Authenticator
	// externalAuthRoutes 是外部认证登录面路由（OIDC/LDAP，roadmap 11.7-⑥）：装配层
	// 注入的 ExternalAuthHandlers.Routes() 收集，注册时同时挂主 mux 与隧道内层
	// localMux（浏览器隧道模式下登录页可达）。nil = 未配置（零回归：无外部端点）。
	externalAuthRoutes []authn.ExternalAuthRoute
	// insecureLoopbackRole 是无认证回环兜底合成的 Principal 角色（装配/测试瞬态；
	// 空 ⇒ 默认 user，生产零影响）。管理员类 UI/接口的 e2e 需要它以 admin 运行：
	// 审计读面是 admin-only（M1-11），否则扫描器/测试无法覆盖审计面板。
	insecureLoopbackRole string
	// allowInsecureLoopback 是无认证兜底开关（读取优先级：opts 注入 > cfg 配置）。
	// 仅调试语义：ring 为空时放行 loopback 来源（见 handleNoCredentials）。
	allowInsecureLoopback bool
	// registerLimiter 是公开注册端点的独立限频器（复用 pkg/server/ratelimit.RateLimiter，
	// per-IP 令牌桶 + 全局滑动窗口，5/min 封顶，D6/M1）。注册是唯一用户入口（U3）
	// 且不经 authMiddleware——蓄意攻击者可任意 IP 洪泛，故逐 IP 限频 + 全局窗口
	// 兜底。与文件传输/信令限流（rateLimiter/signalPostRL）隔离配额。nil = 启动未装配。
	registerLimiter *ratelimit.RateLimiter
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
	totpLimiter *ratelimit.RateLimiter
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
	loginLimiter *ratelimit.RateLimiter
	// totpPending 是 TOTP 注册 pending 表（两段式提交）：register 只生成 pending
	// （AK + TOTP secret + owner + 过期时间），**不写 ring / 不落盘**；客户端用正确
	// TOTP 动态码登录成功才提交（AddRegistration + persist）。
	//   - 首 admin 单槽：无 admin 时 pending 表只允许一条（防并发 pending 抢 admin）；
	//   - owner 幂等：同 owner 已有活跃 pending → 409（不产生第二个候选）；
	//   - 绑定失败自动回收：TTL 过期 / 登录失败达阈值 → 删 pending（AK 可复用）；
	//   - 纯内存态（重启即清，与 nonce 池同生命周期），TTL 默认 10 分钟。
	totpPending *totpPendingTable
}

// filesDeps 是文件域（files.Service/分块/上传锁/去重/带宽）域依赖。
type filesDeps struct {
	// metaSvcForTest 是元数据补录 handler 的测试替身（构造注入，运行期不可变；生产恒 nil）。
	metaSvcForTest metaUpdateService
	uploadingFiles sync.Map // map[string]string — filename → uploadID，追踪正在上传的文件名
	// uploadingLockAt 记录**文件级锁标记**的获取时间（key 与 uploadingFiles 同键空间）。
	// 用于 TTL 回收：持锁者异常退出（panic 逃出 defer / 无超时的远端写永久阻塞）时锁条目
	// 永不被释放 ⇒ 该 rel 后续所有写请求持续 409 至重启（评审 P2）。仅锁标记写入本表；
	// 分块会话条目（value=upload_id）不写（它们由 session 存在性判定过期）。
	uploadingLockAt  sync.Map                               // map[string]time.Time
	directLinkWarned sync.Map                               // map[string]struct{} — 已告警「direct_link 被完整性承诺抑制」的卷名
	uploadingStop    chan struct{}                          // 关闭后通知 uploadingFiles 定期清理 goroutine 退出（兼容旧装配路径）
	checksumStores   map[string]checksum.ChecksumStoreIface // 按 owner 缓存 checksum 台账（本地/集群后端同缓存）
	uploadStores     map[string]*files.UploadStore          // 按 owner 缓存 per-tenant 分块上传存储（懒创建）
	dedupStores      map[string]*files.DedupStore           // 按 owner 缓存 per-tenant 去重台账（懒创建）
	// filesSvc 是文件服务域实例（pkg/files）。经 fileService() 懒装配：文件服务域只
	// 依赖 h 的窄能力（见 filesRuntime），构造时机不影响语义，而 *Handlers 有多条构造
	// 路径（RegisterRoutes 正式装配、测试手工构造），懒装配让两条路径都无需改动。
	filesSvc     *files.Service
	filesOnce    sync.Once
	filesMu      sync.Mutex
	eventsBus    *EventBus
	eventBusOnce sync.Once
	// bwBuckets 是带宽限速 per-owner 令牌桶缓存（rate_limit.bandwidth 启用时懒建）。
	bwBuckets sync.Map
	// bwCoordOnce / bwCoord 是带宽跨实例协调器（coord_backend=file）单例缓存。
	bwCoordOnce sync.Once
	bwCoord     *byteFileCoordinator
}

// volumeDeps 是卷域（卷集合/配额/容量/镜像/分层/保留/轮转）域依赖。
type volumeDeps struct {
	// stateCache 是卷状态缓存（未启用 = nil；与卷视图装饰器同处装配）。仅用于显式
	// refresh 入口：按卷清空快照（外部卷被外部改动后手动收敛）。
	stateCache *volumestate.Cache
	// rebalanceProg 是卷再平衡迁移进度状态（roadmap 3.3 P1 残余：#440 后补）。
	// 互斥保护；rebalanceVolumeHandler 循环内更新，/metrics 输出 sproxy_rebalance_progress
	// gauge（按 from/to 维度，0-100 百分比）。nil = 未装配（进度不可观测，零回归）。
	rebalanceProg *rebalanceProgress
	// externalHealth 是外部卷健康探测缓存（roadmap 3.3 P1 后端健康探针）：
	// map[卷名]{state, at}，30s TTL 内复用上次探测结果（惰性：列表触发，无后台 goroutine）。
	// healthMu 串行化读写（列表并发安全）。nil = 未装配探针（零回归）。
	externalHealth map[string]externalHealthEntry
	healthMu       sync.Mutex
	storageMgr     *capacity.StorageManager
	// mirrorStop / mirrorWg 是卷镜像周期 goroutine 的停止信号与等待组
	// （仅 cfg.MirrorInterval > 0 时挂载；与 versionGC 同构）。
	mirrorStop chan struct{}
	mirrorWg   sync.WaitGroup
	// indexSaveStop / indexSaveWg 是搜索索引快照周期保存 goroutine 的停止信号与等待组
	// （仅 cfg.IndexSaveInterval > 0 时挂载；与 mirror 同构）。
	indexSaveStop chan struct{}
	indexSaveWg   sync.WaitGroup
	// tierStop / tierWg 是冷热分层自动降级周期 goroutine 的停止信号与等待组
	// （仅 cfg.TierPolicy.Interval > 0 时挂载；与 mirror 同构）。
	tierStop chan struct{}
	tierWg   sync.WaitGroup
	// retentionStop / retentionWg 是卷级保留期清理周期 goroutine 的停止信号与等待组
	// （仅任一卷 volumes[].retention.gc_interval > 0 时挂载；与 mirror 同构，roadmap 11.7-⑨）。
	retentionStop chan struct{}
	retentionWg   sync.WaitGroup
	// rotationStop / rotationWg 是凭据自动轮换周期 goroutine 的停止信号与等待组
	// （仅 credentials.rotation.interval > 0 时挂载；与 versionGC 同构）。
	rotationStop chan struct{}
	rotationWg   sync.WaitGroup
	// 多租户存储布局装配（任务 4，供 P2/P3 各 handler 迁移复用）。
	// globalRoot 是 OpenRoot 后的全局存储根（含 LAYOUT_VERSION 校验）；
	// globalPool 是全局配额池（cfg.MaxStorageBytes 兜底）。租户/checksum/配额均懒创建并缓存，
	// tenantMu 串行化懒创建（无竞态）；P2 各 handler 逐个切换到 tenantOf/checksumStoreFor/quotaFor。
	globalRoot   *storage.Root                      // 全局存储根（OpenRoot + LAYOUT_VERSION）
	globalPool   *quota.Pool                        // 全局配额池（cfg.MaxStorageBytes 兜底）
	tenants      *storage.TenantCache               // 按 owner 缓存租户（含 anonymous；懒创建）
	quotaScopes  map[string]*quota.Scope            // 按 owner 缓存配额 Scope（globalPool.Scope 懒创建）
	quotaBuckets map[string]map[string]*quota.Scope // 按 owner 缓存功能桶配额子 Scope（user/cloud/archive/chunk/version）
	// stagingScopes 按 owner 缓存**独立 staging 配额 Scope**（用户裁定 2026-10-10：
	// 本地暂存独立记账，不与网盘 owner_quotas 混用——外部卷 WriteFile 先预留本地字节）。
	// 上限 = trusted_volume.staging_quota_bytes；0 = 不限制但记账。tenantMu 保护。
	stagingScopes map[string]*quota.Scope
	// stagingPool 是 staging 的**独立根池**（M5，第 7 轮 MAJOR）：不挂 globalPool 之下，
	// 只受 trusted_volume.staging_quota_bytes 约束（0 = 该维度不限）——否则慢速外部卷上传
	// 长期占暂存会沿父链抬高 max_storage_bytes 的全局 usage，误拒无关 owner 的正常写。
	stagingPool *quota.Pool
	// stagingTrackers 按 owner 缓存 staging 等待队列（quota.StagingTracker，含 sync.Cond）。
	// **必须复用同一实例**：每请求新建 tracker 会产生各自独立的 cond，其它请求的
	// ReleaseUsage 广播无法唤醒等待者（只能干等满 deadline，甚至误报「空间不足」；
	// P2 对抗评审）。tenantMu 保护。
	stagingTrackers map[string]*quota.StagingTracker
	tenantMu        sync.Mutex // 串行化 checksumStores/uploadStores/quotaScopes/quotaBuckets 懒创建
	// quotaWaitTimeout 是写路径「配额不足 ⇒ 排队重试」的等待上限（**0 = quota.DefaultWaitTimeout**，
	// 即用户裁定 2026-10-14 的 30s 语义）。**仅供测试夹具注入小值**（生产恒 0，语义不变）——否则
	// 大量「请求能放下、但当前用量不够」的用例各自白等 30s，CI 实测累计触发 5min 包超时。
	quotaWaitTimeout time.Duration
	// volSet 是装配后的卷集合（RegisterRoutes 装配；nil = 未装配卷功能的旧装配路径，如
	// 测试手工构造的 Handlers）。默认卷语义：globalRoot 字段 = 默认卷根、tenantFor 走默认卷。
	// 写路径本任务仍只走默认卷（T4 起卷感知），volSet 供多卷 reconcile 与后续卷路由消费。
	volSet *registry.Set
	// userVolumes 是用户自有卷 store（U3：per-owner volume meta 持久化；nil = 未装配，
	// 相关 /api/volumes/user 路由返回 400）。**它是运行时用户卷的唯一事实源**（重启由
	// ScanRestore 恢复；历史 `configWriter` YAML 写回机制已随阶段 2 迁移移除）。
	userVolumes *UserVolumeStore
}

// cloudDeps 是云下载域（任务管理器/URL 解析）域依赖。
type cloudDeps struct {
	cloudMgr *cloud.CloudDownloadManager
	// urlResolver 是通用 URL 解析注册表（POST /api/resolve）；nil = 未装配（503）。
	urlResolver ResolverRegistry
	// cloudService 是云任务编排核心的**单实例**（cloudTaskService 懒构造一次后并发复用）。
	// 单实例是 P1-c S3c 的语义要求：归档桶占用登记（archiveUsage）已迁入 cloudtasks.Service，
	// 记录（单任务/批量/组归档）与删除（组删除联动）必须同实例才能按登记释放。
	cloudService     *cloudtasks.Service
	cloudServiceOnce sync.Once
}

// shareDeps 是分享域域依赖。
type shareDeps struct {
	shareStore ShareStoreIface
}

// hubDeps 是Hub/Mesh 域（路由表/DHT/联邦/信令/写门）域依赖。
type hubDeps struct {
	// xferMetrics 是传输层扩展指标提供者（RegisterRoutesOpts.XferMetrics；nil = 不输出）。
	xferMetrics XferMetricsProvider
	routeTable  *hub.MeshRouteTable
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
	// meshRuntimeInfo 是跨节点面/角色的**运行态**提供者（装配层注入，见 mesh_status.go）。
	// nil = 未注入（视图只反映配置态）。
	meshRuntimeInfo func() MeshRuntimeInfo
	// writeGuard 是集群写面门（roadmap 12.1-2 只读副本接入；nil = 未装配写面全放行
	// 零回归）。由 RegisterRoutesOpts.WriteGuard 注入（cmd/sproxy cluster.enabled 时）。
	writeGuard *leader.WriteGuard
}

// clusterDeps 是集群/同步域（节点注册表/同步任务/冲突索引/状态存储）域依赖。
type clusterDeps struct {
	stateStore state.StateStore // 状态存储后端（statestore.md §5.2；nil = 未装配零回归）
	// nodeRegistry 是集群节点注册表（roadmap 11.11 方案 A-⑤；nil = 单节点零回归）。
	nodeRegistry  *NodeRegistry
	syncMgr       *syncmgr.Manager       // 文件同步任务管理器（nil = 未配置 sync，相关路由返回 400）
	conflictIndex *syncmgr.ConflictIndex // 冲突索引（merge3 冲突 API 数据源；nil = 未装配，相关路由 400）
}

// auditDeps 是审计域（logger/服务/合规段族）域依赖。
type auditDeps struct {
	// auditLogger 是操作审计专用 logger：固定 JSON 格式（不随 log_format 切换）、
	// 与业务 logger 独立，保证审计行可机器检索。RegisterRoutes 初始化；测试可经
	// RegisterRoutesOpts.AuditLogger 注入 buffer 捕获。
	auditLogger *slog.Logger
	// auditSvc 是**唯一审计服务**（pkg/audit）：条目写入、内存活跃索引、轮转与策略。
	// 由 setupAuditServices 装配；cfg.Audit.BufferSize=0 ⇒ nil（审计关闭，读面返回空表）。
	auditSvc *audit.Service
	// auditJournal 是**合规段族**落盘后端（全量历史，供 /api/audit 与导出读取）。
	// 打开失败 ⇒ nil（降级为仅内存索引），审计绝不阻断启动。
	auditJournal *audit.FileJournal
}

// aiDeps 是AI 与告警域（通知中心/洞察/隐私/事件消费/告警引擎）域依赖。
type aiDeps struct {
	// notifyCenter 是通知中心（roadmap P0；cfg.Notify.Enabled 时由 RegisterRoutes
	// 装配）。RecordAudit 末尾 dispatch（异步 goroutine，绝不阻塞审计/业务）。
	// nil = 未启用（零回归）。
	notifyCenter *NotifyCenter
	// aiInsight 是 AI 文件洞察（roadmap 11.9-⑤；cfg 装配，nil = 未启用零回归）。
	aiInsight *AIInsight
	// aiPrivacy 是 AI 派生数据隐私（roadmap 11.9-⑧；cfg 装配，nil = 未启用零回归）。
	aiPrivacy *AIPrivacy
	// aiEventConsumer 是 AI 事件流水线消费端（roadmap 12.2-4；cfg 装配，nil = 未启用零回归）。
	aiEventConsumer *AIEventConsumer
	// alertEngine 是阈值告警引擎（roadmap P1；cfg.Alerts.Enabled 时由 RegisterRoutes
	// 装配 + Start 轮询）。卷 degraded / 登录锁定 / 同步失败挂点；nil = 未启用零回归。
	alertEngine *AlertEngine
}

// opsDeps 是运维域（调度器/巡检/用量账本）域依赖。
type opsDeps struct {
	// scheduler 是统一任务调度器（roadmap 11.10-H2）：uploading 清理 / version-gc /
	// trash-gc / share-cleanup 四个周期任务统一注册（RegisterRoutes 装配；
	// nil = 未装配的旧构造路径，Close 跳过）。
	scheduler *Scheduler
	// verifyBusy 是全仓 checksum 巡检的单飞防重入标记（roadmap 11.3-⑩）：
	// 上一次巡检未结束时，新的触发直接跳过（Concurrent=true，不堆叠执行）。
	// 与统一调度器（#574）的 CAS 语义同构；定时任务与手动端点共用同一入口。
	verifyBusy atomic.Bool
	// verifyHook 是测试注入的巡检执行钩子（生产恒 nil；并发用例阻塞制造重入窗口）。
	verifyHook func()
	// usageStore 是计量报告 per-owner 周期用量聚合存储（roadmap 11.10-⑩ 片 2；
	// usage.enabled=true 时由 setupUsageReport 装配，nil = 未启用零回归——
	// /api/usage/report 400、/metrics 仅 owner 计数不落盘）。
	usageStore *usageStore
}
