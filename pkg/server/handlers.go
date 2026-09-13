// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
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
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
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
	auditLogger *slog.Logger
	// auditRing 是有界内存环形审计缓冲（cfg.Audit.BufferSize，默认 2048；0=关闭）。
	// RegisterRoutes 按 cfg 装配（BufferSize>0 时创建）；RecordAudit 在 TS 填充后
	// 挂钩 Add，所有审计录入点自动进 ring。nil = 关闭（GET /api/audit 返回空表）。
	auditRing      *AuditRing
	cloudMgr       *cloud.CloudDownloadManager
	syncMgr        *syncmgr.Manager // 文件同步任务管理器（nil = 未配置 sync，相关路由返回 400）
	storageMgr     *capacity.StorageManager
	uploadingFiles sync.Map             // map[string]string — filename → uploadID，追踪正在上传的文件名
	uploadingStop  chan struct{}        // 关闭后通知 uploadingFiles 定期清理 goroutine 退出
	uploadingWg    sync.WaitGroup       // 等待 cleanupUploadingFilesLoop 退出
	closeOnce      sync.Once            // 防止 Close() 重复关闭 channel
	noncePool      *sproxysig.NoncePool // SproxySig nonce 防重放池
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

	// filesSvc 是文件服务域实例（pkg/files）。经 fileService() 懒装配：文件服务域只
	// 依赖 h 的窄能力（见 filesRuntime），构造时机不影响语义，而 *Handlers 有多条构造
	// 路径（RegisterRoutes 正式装配、测试手工构造），懒装配让两条路径都无需改动。
	filesSvc  *files.Service
	filesOnce sync.Once
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
			files.WithAudit(rt),
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
		bucketLimits = cfg.BucketLimits
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
	// TODO: ctx 当前未使用，后续可用于 graceful shutdown 或请求级超时控制
	srvMux := opts.Mux
	cfg := opts.CfgPtr.Load()
	log := defaultLogger(opts.Logger)
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
		h.authenticators = []Authenticator{NewRingAuthenticator(h.credentialRing, h.noncePool)}
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
	localMux.HandleFunc("PUT /api/config", h.updateConfigHandler)
	// 审计查看：隧道内层注册（无 authMiddleware——隧道加密即认证，与 /api/shares、
	// /api/stats 的 localMux 侧同模式）。auditHandler 只读 ring 回 JSON，自身不做
	// 签名校验。浏览器隧道模式下用户面操作必须隧道可达（仅注册主 mux 会 404）。
	localMux.HandleFunc("GET /api/audit", h.auditHandler)
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

// bootstrapCredentials 装配凭据 Ring 与关联 store（RegisterRoutes 启动时调用一次）：
//   - 显式注入（opts.CredentialRing）→ 直接使用；
//   - 否则从 opts.CredentialStore 载入快照（真实/损坏处理见 CredentialStore.Load）；
//   - **U3：零凭据启动**——不再生成首启 anonymous 凭据（4A 的 generateBootstrapCredential
//     路径已移除）。store 为空 = 系统以零凭据等待注册：register 公开端点是唯一用户
//     入口，首个经回环注册的用户由 AddRegistration 原子授 admin（DEC-F/D2）。
//     空 store 时记启动日志提示「首次注册经回环，将成为 admin」（S2）。
func (h *Handlers) bootstrapCredentials(opts RegisterRoutesOpts) {
	store := normalizeStorer(opts.CredentialStore)
	if opts.CredentialRing != nil {
		h.credentialRing = opts.CredentialRing
		h.credentialStore = store
		return
	}
	ring := accesskey.NewRing()
	if store != nil {
		if keys, err := store.Load(); err != nil {
			h.logger.Error("载入凭据 store 失败（fail-closed：拒绝启动，防止用空凭据表运行）", "error", err)
			panic("载入凭据 store 失败: " + err.Error())
		} else if len(keys) > 0 {
			if rerr := ring.Replace(keys); rerr != nil {
				h.logger.Error("重建凭据 Ring 失败（fail-closed）", "error", rerr)
				panic("重建凭据 Ring 失败: " + rerr.Error())
			}
			h.logger.Info("已从凭据 store 载入", "keys", len(keys))
		}
	}
	// U3：零凭据等待注册——store 为空即不生成任何凭据（ring.Len()==0），
	// 首个注册者（回环）由 register 端点经 AddRegistration 原子授 admin。
	if ring.Len() == 0 {
		h.logger.Info("零凭据启动：首次注册请在本机回环执行 /api/credentials/register，首位注册者将成为 admin")
	}
	h.credentialRing = ring
	h.credentialStore = store
}

// BootstrapServerCredentials 是生产装配入口：为服务端准备凭据 Ring + store
// （供 cmd/sproxy 在 RegisterRoutes 与 hub 装配之前调用，随后把二者注入 opts）。
//   - store = <默认卷根>/anonymous/meta/credentials.json（服务端级全局凭据，anonymous
//     租户的 meta 桶；多租户部署如需 per-owner 凭据经 /api/credentials 管理，见任务 5）。
//     **默认卷根经 resolveDefaultVolumeRoot 裁决**（非 cfg.StorageRoot）——显式
//     volumes[0].root ≠ storage_root 分叉时凭据必须落默认卷 meta（AD-5 meta 归属默认卷
//     不变式；否则重启凭据 Ring 丢失，PR-B 终审建议 9）。
//   - 载入既有快照；**U3：不再生成首启 anonymous 凭据**——store 为空则返回空 Ring，
//     系统以零凭据等待 register 公开端点（首个回环注册者授 admin）。
func BootstrapServerCredentials(cfg *Config, logger *slog.Logger) (*accesskey.Ring, accesskey.CredentialStorer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	metaDir := filepath.Join(resolveDefaultVolumeRoot(cfg), anonymousOwner, "meta")
	var store accesskey.CredentialStorer = accesskey.NewCredentialStore(metaDir)
	// 4C-2：credential_store.encrypt=true 时把凭据文件包装为加密静态存储
	// （EncryptingStorer，按 backend 选 SecureStorer）——cmd 与 opts 注入面不变
	// （返回类型已是 CredentialStorer 接口，替换实现无缝）。默认关 = 明文零回归。
	// backend：aesgcm（缺省/空）= 本地 AES-256-GCM master key；vault = Vault Transit
	// （密钥不出 Vault）。token/凭据不落日志。
	if cfg.CredentialStore.Encrypt {
		storePath := filepath.Join(metaDir, "credentials.json")
		// backend 是规范化后的日志值：直接 Config{Backend:""}（未经 SetDefaults）走 aesgcm
		// 分支时 cfg.Backend 为空串，日志应仍记 "aesgcm"（M-1）。
		backend := "aesgcm"
		var secure accesskey.SecureStorer
		switch cfg.CredentialStore.Backend {
		case "vault":
			backend = "vault"
			tok, err := resolveVaultToken(cfg.CredentialStore.Vault)
			if err != nil {
				return nil, nil, err
			}
			v, err := accesskey.NewVaultTransitStorer(accesskey.VaultOptions{
				Addr:     cfg.CredentialStore.Vault.Addr,
				Mount:    cfg.CredentialStore.Vault.Mount, // SetDefaults 已填 "transit"
				KeyName:  cfg.CredentialStore.Vault.KeyName,
				Token:    tok,
				CAFile:   cfg.CredentialStore.Vault.CAFile,
				Timeout:  cfg.CredentialStore.Vault.Timeout, // SetDefaults 已填 10s
				CacheTTL: cfg.CredentialStore.Vault.CacheTTL,
				// I-2：AAD context 绑 owner 唯一相对 storage_root 路径（匿名租户全局凭据
				// 文件）——同 vault mount+key 下不同凭据文件 context 各不相同，密文被复制/
				// 搬移到另一文件即 decrypt 失败（防跨节点/租户搬移）。filepath.ToSlash 归一
				// 跨平台路径分隔符，防 Windows 反斜杠导致 AAD 不一致。
				AADPath: filepath.ToSlash(filepath.Join(anonymousOwner, "meta", "credentials.json")),
			})
			if err != nil {
				return nil, nil, err
			}
			// 启动探活（F1）：POST /v1/auth/token/lookup-self 同时验可达性 + token 有效性。
			// 空 store 首启也探（Load 无密文不发 Vault 请求）——防配错 Vault 静默启动到首写才炸。
			if perr := v.Probe(); perr != nil {
				return nil, nil, fmt.Errorf("credential_store.backend=vault 启动探活失败（可达性或 token 有效性）: %w", perr)
			}
			secure = v
		default: // aesgcm（含空 = 向后兼容）
			masterKey, err := resolveCredentialMasterKey(cfg)
			if err != nil {
				return nil, nil, err
			}
			secure = accesskey.AESGCMStorer{Key: masterKey}
		}
		store = accesskey.NewEncryptingStorer(storePath, secure)
		logger.Info("凭据静态存储加密已启用", "backend", backend)
	}
	ring := accesskey.NewRing()
	if keys, err := store.Load(); err != nil {
		return nil, nil, fmt.Errorf("载入凭据 store 失败（fail-closed）: %w", err)
	} else if len(keys) > 0 {
		if rerr := ring.Replace(keys); rerr != nil {
			return nil, nil, fmt.Errorf("重建凭据 Ring 失败: %w", rerr)
		}
		logger.Info("已从凭据 store 载入", "keys", len(keys))
	}
	// U3：不生成 anonymous——空 store = 零凭据等待注册。
	if ring.Len() == 0 {
		logger.Info("零凭据启动：请在本机回环执行 /api/credentials/register，首个注册者将成为 admin")
	}
	return ring, store, nil
}

// resolveCredentialMasterKey 解析 credential_store.encrypt=true 装配所需的 32B master
// key（单一事实源 = accesskey 的 LoadMasterKeyFromFile / MasterKeyFromBase64，本层只做
// 读取与来源选择，不写 AES/HKDF）。来源顺序：master_key_file 文件 > 环境变量
// CredentialMasterKeyEnv；两者都无 → error（fail-fast，防启动后解密失败用空凭据表运行）。
func resolveCredentialMasterKey(cfg *Config) ([]byte, error) {
	if cfg.CredentialStore.MasterKeyFile != "" {
		key, err := accesskey.LoadMasterKeyFromFile(cfg.CredentialStore.MasterKeyFile)
		if err != nil {
			return nil, fmt.Errorf("读取 credential_store.master_key_file 失败: %w", err)
		}
		return key, nil
	}
	if v := os.Getenv(CredentialMasterKeyEnv); v != "" {
		key, err := accesskey.MasterKeyFromBase64(v)
		if err != nil {
			return nil, fmt.Errorf("解析环境变量 %s 失败: %w", CredentialMasterKeyEnv, err)
		}
		return key, nil
	}
	return nil, fmt.Errorf("credential_store.encrypt=true 需配置 credential_store.master_key_file 或环境变量 %s（base64 编码 32B master key）", CredentialMasterKeyEnv)
}

// resolveVaultToken 解析 backend=vault 装配所需的 Vault token（来源顺序：token_file 文件
// （读入后 TrimSpace）> TokenEnv 环境变量 > error fail-fast）。token 值只用于构造
// VaultTransitStorer（HTTP 头），不落日志。
func resolveVaultToken(vc VaultConfig) (string, error) {
	if vc.TokenFile != "" {
		data, err := os.ReadFile(vc.TokenFile)
		if err != nil {
			return "", fmt.Errorf("读取 vault token 文件失败: %w", err)
		}
		tok := strings.TrimSpace(string(data))
		tok = strings.TrimPrefix(tok, "\uFEFF") // 清 UTF-8 BOM（Windows 编辑的 token 文件常带，否则 403 难排查）
		if tok != "" {
			return tok, nil
		}
	}
	envName := vc.TokenEnv
	if envName == "" {
		envName = "VAULT_TOKEN"
	}
	if tok := os.Getenv(envName); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("credential_store.backend=vault 需配置 token_file 或环境变量 %s", envName)
}

// bestFirstCredential 返回 Ring 中首个可用（alive）AK 及其 64-hex SK。
// 供 xfer listener 装配（取代 cfg.AccessKeys[0]）使用。ring 为空 / 无可存活着
// 返回 ("", "", false)。
func bestFirstCredential(ring *accesskey.Ring) (ak, skHexStr string, ok bool) {
	if ring == nil {
		return "", "", false
	}
	for _, k := range ring.Snapshot() {
		if e := ring.CoreEntry(k.AK); e != nil {
			return k.AK, skHex(e.SK), true
		}
	}
	return "", "", false
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
	// 关闭多租户存储根：先关各租户子根（默认卷缓存的租户子根；非默认卷的随 volSet.Close），
	// 再关卷集合根（含默认卷根 = globalRoot）。置 nil 防重复 Close。volSet == nil（手工构造的
	// 旧装配路径）回落直接关 globalRoot（既有行为）。
	_ = h.tenants.Close()
	if h.volSet != nil {
		_ = h.volSet.Close()
		h.volSet = nil
		h.globalRoot = nil
	} else if h.globalRoot != nil {
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
	stores := make([]*files.UploadStore, 0, len(h.uploadStores))
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
// 作为 goroutine 在 RegisterRoutes 中启动，由 Close() 通过关闭 uploadingStop 停止；单次清理
// 委托 cleanupUploadingFilesPass（独立可测）。
func (h *Handlers) cleanupUploadingFilesLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.uploadingStop:
			return
		case <-ticker.C:
			h.cleanupUploadingFilesPass()
		}
	}
}

// cleanupUploadingFilesPass 执行一轮 uploadingFiles 过期清理。
// 锁标记条目（upload/move/txn，见 isUploadingLockMarker）都无对应 session，直接跳过
// （若把 "move" 当 upload_id 查 GetSession("move")==nil 会误删锁条目：超 10 分钟的长
// move/delete/restore/complete 持锁被清理 → 同 rel 并发操作越过锁，T6c 修复轮建议 1）。
func (h *Handlers) cleanupUploadingFilesPass() {
	h.uploadingFiles.Range(func(key, value any) bool {
		filename, ok := key.(string)
		if !ok {
			return true
		}
		uploadID, ok := value.(string)
		if !ok {
			return true
		}
		if isUploadingLockMarker(uploadID) {
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
