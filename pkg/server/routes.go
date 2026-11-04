// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// routes.go 是**路由注册面**：RegisterRoutesOpts 选项结构体、RegisterRoutes（全部 HTTP 端点
// 的装配与挂载，含主 mux 的 SproxySig 认证中间件与 localMux 隧道内层路由），以及路由级辅助
// （localMuxGate / fileRoute；分类查表见 route_class_index.go）。
//
// 拆分说明见 handlers.go 顶部。

package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/authn"
	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/leader"
	"github.com/cocomhub/sproxy/pkg/server/ai"
	"github.com/cocomhub/sproxy/pkg/server/alerts"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/server/metrics"
	"github.com/cocomhub/sproxy/pkg/server/notify"
	"github.com/cocomhub/sproxy/pkg/server/ratelimit"
	"github.com/cocomhub/sproxy/pkg/server/router"
	"github.com/cocomhub/sproxy/pkg/server/share"
	"github.com/cocomhub/sproxy/pkg/server/verify"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
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
	// InsecureLoopbackRole 覆盖无认证回环兜底合成的 Principal 角色（**装配/测试瞬态**，
	// 空 ⇒ user）。仅供 e2e/本地联调需要管理员身份的场景（如审计读面 admin-only）。
	InsecureLoopbackRole string
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
	// QuotaWaitTimeout 是写路径「配额不足 ⇒ 排队重试」的等待上限覆盖（测试瞬态；
	// 0 = 默认 quota.DefaultWaitTimeout 30s）。**仅供测试注入短值**：生产恒 0，语义不变；
	// 否则「配额故意打满断言 507」的用例各白等 30s，批量累计触发包超时。
	// 同构先例：TotpRateLimit/LoginRateLimit（测试装配瞬态，不写入 cfg）。
	QuotaWaitTimeout time.Duration
	// XferMetrics 是传输层扩展指标提供者（装配层注入；nil = 不输出 WS/QUIC
	// 指标行）。cmd/sproxy 经 go.work import ext/ws、ext/quic 提供——pkg/server
	// 不直接依赖独立 module（web/e2e 等子 module 经 pkg/server 间接编译不破）。
	XferMetrics metrics.XferMetricsProvider
	// CloudExitDial 是云端下载的经 mesh 出口拨号函数（装配层注入；nil = 服务端本地直连）。
	// 非 nil 时覆写 cloud 下载器的 Transport.DialContext（本地直连优先 → 失败回退经出口节点）。
	// cmd/sproxy 在 cloud_download_exit_node 配置启用时用 newMeshHubClient + mesh 路由构造。
	CloudExitDial func(ctx context.Context, addr string) (net.Conn, error)
	// ExternalAuthHandlers 是外部认证（OIDC/LDAP）登录面宿主嵌入点（roadmap 11.7-⑥）：
	// ext/oidcldap 提供的 Provider 实现 pkg/authn.ExternalAuthHandler（独立 go module，
	// pkg/server 不 import ext module——与 XferMetrics/CloudExitDial 注入同构）。
	// 装配语义：
	//   - Routes() 返回的登录/回调端点**同时注册**主 mux 与隧道内层 localMux（浏览器
	//     隧道模式下登录页可达）；
	//   - Authenticators（由各外部认证器实现 pkg/authn.Authenticator）追加到认证链
	//     **尾部**（RingAuthenticator 在前、外部在后——外部凭据格式（Bearer/会话
	//     cookie）与 SproxySig 互斥，先后无冲突）；
	//   - 未配置（nil）= 零回归：无外部端点、认证链默认链不变。
	ExternalAuthHandlers []authn.ExternalAuthHandler
	// WriteGuard 是集群写面门（roadmap 12.1-2 只读副本接入）：非 nil 时写面路由
	// （文件写子组 + cloud/trash/verify/config/credentials/volumes/notify/ai/sync）
	// 前置 Authorize()——非主节点 ErrNotLeader → 503。nil = 未装配（单节点零回归，
	// 写面全放行）。由 cmd/sproxy 在 cluster.enabled 时装配
	// （LocalLeaderElector + NewWriteGuard，replica 角色恒 follower）。
	WriteGuard *leader.WriteGuard
	// StateStore 是状态存储后端（statestore.md §5.2）：非 nil 时分享/索引适配器切
	// StateStore 后端（双读单写零回归）；nil = 未装配（分享/索引走原本地 JSON 落盘）。
	StateStore state.StateStore
	// RemoveMovedSource 是 move 删源实现（nil = 默认 storage.Root.Remove）。**测试注入
	// opt（用户裁定 2026-10-07：禁止测试依赖全局函数/并发修改包级 seam）**：构造时注入
	// Handlers 实例字段，运行期不可变；测试模拟「并发 delete 已删源」的 IsNotExist 竞态。
	RemoveMovedSource func(root *storage.Root, rel string) error
	// URLResolver 是通用 URL 解析注册表（POST /api/resolve 用）。nil = 不装配解析接口
	// （/api/resolve 返回 503）。cmd/sproxy 在 pikpak 注册后注入
	// downloader.DefaultRegistry（含 pikpak 分享解析扩展）。
	URLResolver ResolverRegistry
}

// ResolverRegistry 是 /api/resolve 依赖的最小接口（避免 pkg/server 直接 import
// 具体注册表类型；满足 downloader.Registry 即可）。
type ResolverRegistry interface {
	ResolveURL(ctx context.Context, rawURL string) (*downloader.ResolveInfo, error)
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

	// 审计服务装配：有界内存环形缓冲 + 落盘存储（BufferSize>0 时启用；打开失败
	// 降级 ring-only，审计绝不阻断启动）。详细语义见 setupAuditServices。
	// 冷段归档目的地**延迟解析**：可能是**卷**（`audit.archive_volume`）⇒ 需卷集合就绪后
	// 才能拿到卷 FS。assemble 期先拿一个闭包，卷集合装配完成后再回填（轮转归档在运行期触发 ✓）。
	var coldVolSet *registry.Set
	// 指标实例提前创建：审计装配（journal/service）与 Handlers 共用同一实例 ⇒ `audit_dropped`
	// 等审计计数能经 `/metrics` 暴露（评审 R13 P1：此前无人注入 ⇒ 恒 NullMetrics）。
	mx := metrics.NewMetrics()
	auditSvc, auditJournal := setupAuditServices(cfg, func() syncpkg.FS {
		return newColdArchiveFS(cfg, coldVolSet, log)
	}, newAuditMetrics(mx), log)

	// 卷集合装配（多卷，任务 3）：按 cfg.Volumes 逐卷 MkdirAll + storage.OpenRoot
	// （LAYOUT_VERSION 写入/校验）+ 卷容量 Pool + ACL 解析。缺省形态（YAML 只配
	// storage_root 未配 volumes）由 resolveDefaultVolumeRoot 裁决为 cfg.StorageRoot——
	// F1 门禁：Volumes[0].Root 占位（defaultStorageRoot）时不按它建根，防存储根静默漂移
	// 到 ./storage（单卷零回归红线）。失败（目录无法打开 / 布局版本不匹配）是致命装配
	// 错误：记 Error 并 panic 拒绝启动，绝不静默继续（否则文件服务会在错误的存储根上运行）。
	warnIfVerifySkipped(cfg, log)
	vs, err := assembleVolumes(cfg, log)
	if err != nil {
		log.Error("卷集合装配失败", "error", err)
		panic("卷集合装配失败: " + err.Error())
	}
	// 卷集合就绪 ⇒ 冷段归档装配（单一入口：解析目的地 + 配置校验 + 启动期归档一次）。
	coldVolSet = vs
	wireAuditColdArchive(cfg, vs, auditJournal, log)

	// U2：装配期注入卷视图装饰器（`VolumeView` 的唯一取卷入口）——本地卷也经同一
	// recipe（trusted.Guard(Wrap) + capacity）成为可信卷；staging 是 per-owner 瞬态，
	// 由消费方 `ApplyStagingQuota` 在取值时套（registry 不 import server，避免成环）。
	// 卷状态缓存（唯一装配点，设计 2026-10-28-volume-state-maintenance）：
	// 未启用 ⇒ nil（零回归）；启用 ⇒ 为非本地卷在**最外层**套「读缓存 + 写自动失效」。
	stateCache := newVolumeStateCache(cfg, vs, log)
	vs.SetVolumeViewDecorator(func(name string, use registry.ViewUse, raw syncpkg.FS) syncpkg.FS {
		switch use {
		case registry.ViewMeta:
			return trusted.Guard(raw)
		default: // ViewUser
			return wrapVolumeState(stateCache,
				name, use, trusted.Guard(trusted.Wrap(raw, trusted.Options{Logger: log})))
		}
	})

	h := &Handlers{
		cfgPtr:            opts.CfgPtr,
		version:           opts.Version,
		buildAt:           opts.BuildAt,
		logger:            log,
		auditLogger:       auditLogger,
		metrics:           mx,
		rebalanceProg:     metrics.NewRebalanceProgress(),
		shareStore:        share.NewStore(log.With("component", "share")),
		routeTable:        opts.RouteTable,
		signalBroker:      NewSignalBroker(opts.RouteTable),
		hubPersist:        opts.HubPersist,
		hubID:             cfg.Hub.NodeID,
		removeMovedSource: opts.RemoveMovedSource, // 测试注入 opt；nil = 默认 storage.Root.Remove
		uploadingStop:     make(chan struct{}),
		noncePool:         sproxysig.NewNoncePool(),
		totpNoncePool:     newTotpNoncePool(),
		tracer:            opts.Tracer,
		auditSvc:          auditSvc,
		auditJournal:      auditJournal,
		notifyCenter:      notify.NewFromConfig(cfg.Notify, log),
		alertEngine:       alerts.NewFromConfig(cfg.Alerts, log),
		// per-AK 失败锁定表（U4）：恒装配（登录端点存在即需；上限 + 惰性清理见
		// loginFailTracker 注释）。
		loginFailTracker: newLoginFailTracker(),
		totpPending:      newTotpPendingTable(),
		// 测试注入空 Ring 时的无认证调试兜底（一次性读取；生产走 cfg.AllowInsecureLoopback）。
		allowInsecureLoopback: opts.AllowInsecureLoopback,
		insecureLoopbackRole:  opts.InsecureLoopbackRole,
		quotaWaitTimeout:      opts.QuotaWaitTimeout,
		volSet:                vs,
		stateCache:            stateCache,
		xferMetrics:           opts.XferMetrics,
		writeGuard:            opts.WriteGuard,
		urlResolver:           opts.URLResolver,
	}
	// **早期装 Root 变异观察者**：必须在 initStorageLayout 创建 anonymous 租户**之前**——
	// 租户子根只在创建时继承观察者，晚装会让已缓存子根永不发事件（见
	// volume_mutation_wiring.go 的两阶段说明）。真正的处理函数在 files.Service 装配后绑定。
	h.installLocalVolumeObservers()

	// 装配多租户存储布局 + 告警引擎（磁盘水位轮询 / 配额预警）。详细语义见
	// initStorageLayout。依赖 Handlers 字面量已注入 volSet/xferMetrics/writeGuard。
	h.initStorageLayout(vs, cfg, log)

	h.signalBroker.SetPersister(opts.HubPersist)

	// 分享链接持久化（§10-③）：分享是服务级资源（token 全局唯一、跨租户可访问），
	// 落盘到 anonymous 租户 meta/share（<默认卷根>/anonymous/meta/share/）。依赖
	// anonymous 租户已预建（上面 tenantFor(anonymousOwner) 检查通过 ⇒ meta 桶存在）。
	// 集群模式切 StateStore 后端（statestore.md §5.2）。详细语义见 initSharePersist。
	h.initSharePersist(opts, cfg, log)

	// 凭据装配（凭据 store 化）：SproxySig 权威表 = Ring。
	//   - opts.CredentialRing 显式注入（测试 / cmd 装配）优先；
	//   - 否则从 opts.CredentialStore 载入快照重建；
	//   - **U3 零凭据启动**——store 为空不再生成首启 anonymous 凭据，系统以零凭据
	//     等待注册：register 公开端点是唯一用户入口，首个经回环注册的用户由
	//     AddRegistration 原子授 admin（DEC-F/D2）。空 store 时 bootstrapCredentials
	//     记启动日志提示「首次注册经回环，将成为 admin」（S2）。
	// storer 归一（typed-nil → nil）在 bootstrapCredentials 内完成（见 normalizeStorer）。
	h.bootstrapCredentials(opts)

	// 凭据自动轮换周期 goroutine、认证链装配（DEC-C）、外部认证（OIDC/LDAP）登录面、
	// 信令收件箱恢复、路由表快照持久化回调。详细语义见 initAuthAndRestore。
	// 依赖 bootstrapCredentials 已装配 credentialRing。
	h.initAuthAndRestore(opts, cfg, log)

	// 统一任务调度器装配：uploading 清理 / share-cleanup / trash-gc / version-gc /
	// checksum-verify 周期任务 + 计量报告 + 搜索索引快照 + 卷镜像 + 冷热分层 +
	// 卷级保留期清理 goroutine。详细语义见 initSchedulerTasks。
	h.initSchedulerTasks(cfg, log)
	// 初始化 StorageManager 和 CloudDownloadManager。详细语义见 initStorageManagers。
	h.initStorageManagers(vs, cfg, log, opts)

	// M3：统一恢复协调器 = **启动恢复的唯一编排点**。放在存储/云下载等子系统装配**之后**
	// （legacy 恢复需要看到已就绪的 manager；此处仍属单线程装配期 ⇒ 无数据竞争）。
	// 顺序：legacy 先（子系统资源就绪）→ 再 WAL 作用域重放。
	// legacy 集合 = 云任务 + 容量账本（本地热账本启动重建，2026-10-22）。
	if coord := setupRecovery(auditSvc, log, nil, append(cloudLegacies(h), ledgerLegacies(h)...)); coord != nil {
		// **同步**执行：与「云任务构造期恢复」的既有语义一致（恢复完成再对外服务），
		// 且不留下跨生命周期的后台 goroutine（否则 Close 后仍可能持有存储文件句柄 ✗——
		// 实测会让测试/运维的目录清理失败）。runStartupRecovery 内以 startupRecoveryTimeout 兜底。
		runStartupRecovery(context.Background(), coord, log)
	}

	// 本地路由子 mux（无 authMiddleware，隧道密钥已提供认证）
	localMux := http.NewServeMux()
	// 已上传文件的元数据补录（方案②：旁路 Extra 更新，读-校验-合并-写 sidecar）。

	// 回收站（roadmap P2）：列表/恢复/清空（隧道内层裸注册同版本管理）。
	// 回收站 API：单一声明点见 routes_trash.go（3 条 × 2 个 mux → 一份表）。
	router.Register(srvMux, localMux, trashRoutePrefix, trashRoutes(),
		func(fn func(*Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { fn(h, w, r) }
		},
		h.routeWrapper)
	// 卷 API（隧道内层裸注册：隧道加密即认证，与版本/share 同模式）
	// 卷备份/导出（roadmap 11.3-②）：导出 = 只读（fileRouteRead / 只读子组）；
	// 导入 = 写（fileRoute / 写子组）。
	// 备份（roadmap 12.2-3 P2）：源=本地卷 → 目标=配置卷（federated 写面）；
	// 隧道内层裸注册（隧道加密即认证，与卷 API 同模式）。
	// 用户卷 API（隧道内层裸注册：与系统卷同模式；CLI --access-key 走此路径）
	// 配额查询**联邦化**（片 5）：用户视图 = 对「该用户可访问的卷」求和（设计 §1.5）。
	// secret 卷管理 API（roadmap/secret 接线）：隧道内层裸注册（隧道加密即认证，同
	// /api/volumes 模式）。secrets 是 ExternalBackend，通用文件 API 的 ?volume= 只解析
	// 本地卷不命中它，故用专用端点管 secret（创建/列表/导出/删除；服务端只校验+落盘，
	// 不参与口令派生）。创建 random 由服务端生成。
	// backend 列表 API（隧道内层裸注册：CLI --access-key 走此路径；供 Web/CLI 动态感知后端）
	// 跨节点面只读运维视图（隧道内层：加密即认证，与 /api/config 同模式）。
	// 审计查看：隧道内层注册（无 authMiddleware——隧道加密即认证，与 /api/shares、
	// /api/stats 的 localMux 侧同模式）。auditHandler 只读 ring 回 JSON，自身不做
	// 签名校验。浏览器隧道模式下用户面操作必须隧道可达（仅注册主 mux 会 404）。
	// 计量报告导出（roadmap 11.10-⑩ 片 2）：隧道内层裸注册（隧道加密即认证，与
	// /api/stats 同模式）；handler 内做 owner 自查询/管理员全量权限裁决。
	// 文件变更事件流（roadmap §2 P1）：SSE 订阅 upload/delete/rename/mkdir/rmdir/version。
	// 隧道内层裸注册（隧道加密即认证，与 audit/share 同模式）；外层经 authMiddleware 保护。
	// 冷段区间读（取证式随机读；管理员门在 handler 内，白名单来自 ListCold）。
	// 通知中心 HTTP 面（roadmap P0 / 11.7-⑦）：路由表由 pkg/server/notify 声明
	// （前缀 + 相对路径 + 两端分类）。历史/渠道自检外部面 auth（SproxySig 验签），
	// feed 是订阅源 ⇒ 公开裸注册（feed_token 门禁在 handler 内，仿 /metrics 语义）；
	// 隧道内层三条均 tunnel（隧道加密即认证）。
	// 文件域路由（upload/download/files/volumes/secrets/backends/versions/tags/batch/
	// archive/search/du/quota）：**单一声明点**见 routes_files.go；此处一次注册到两个
	// mux——Server 侧按分类取 fileRoute/fileRouteRead，Local 侧为 ClassTunnel（裸注册，
	// 角色门由 localMuxGate 收口）。此前 45 条路由 × 2 个 mux = 90 处手工配对。
	router.Register(srvMux, localMux, filesRoutePrefix, fileDomainRoutes(),
		func(fn func(*Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { fn(h, w, r) }
		},
		h.routeWrapper)

	router.Register(srvMux, localMux, notify.RoutePrefix, notify.Routes(notify.Endpoints{
		History: h.notifyHistoryHandler,
		Test:    h.notifyTestHandler,
		Feed:    h.notifyFeedHandler,
	}), bindHandler, h.routeWrapper)

	// AI 文件洞察端点（roadmap 11.9-⑤；未装配（ai.insight.enabled=false）→ 400 零回归）：
	// 路由表由 pkg/server/ai 声明。summarize/tag 需要 ctx→owner 派生与 aiInsight
	// 装配态，闭包构造留在装配层，经 Endpoints 绑定；外部面 quota/privacy 为 public、
	// summarize/tag 不注册（ClassNone），隧道内层全部 tunnel。
	router.Register(srvMux, localMux, ai.RoutePrefix, ai.Routes(ai.Endpoints{
		Summarize: func(w http.ResponseWriter, r *http.Request) {
			h.handleAISummarize(w, r, ownerFromRequest(r), h.aiInsight)
		},
		Tag: func(w http.ResponseWriter, r *http.Request) {
			h.handleAITag(w, r, ownerFromRequest(r), h.aiInsight)
		},
		Quota:        h.handleAIQuota,
		Privacy:      h.handleAIPrivacy,
		PrivacyPurge: h.handleAIPrivacyPurge,
	}), bindHandler, h.routeWrapper)
	// 分块上传/下载路由（本地）

	// 隧道内层中间件链（gzip + 速率限制 + CORS + localMuxGate 角色门禁 +
	// writeGuard 写面门 + requestLog 追踪）与 tunnelHandler/localHandler 装配。
	// 详细语义见 buildTunnelHandlers。
	h.buildTunnelHandlers(localMux, cfg, log)

	// 文件操作路由组（DEC-C）：authMiddleware + requireRole 门禁
	// （upload/download/delete/rename/list/stat/mkdir/rmdir/search/batch/chunk/
	// archive/versions/share…）。RBAC 细分（11.5-①）：只读子组（声明分类
	// ClassReader）走 fileRouteRead = requireRole(reader)
	// （Role∈{reader,user,admin}）；写子组保持 fileRoute = requireRole(user)
	// （Role∈{user,admin}，一字不改零回归）。
	// 全仓 checksum 巡检（roadmap 11.3-⑩）：POST /api/verify 是运维审计面，走
	// authMiddleware（SproxySig 验签）即可——与 /api/stats 同模式，不做文件组门禁
	// （巡检读全卷台账+重算，非 per-file 用户面）。路由表由 pkg/server/verify 声明。
	router.Register(srvMux, localMux, verify.RoutePrefix, verify.Routes(h.verifyHandler),
		bindHandler, h.routeWrapper)
	// checksum 台账运维修复面（§15.1 步骤②）：从对象 meta 重建台账（部分缺失时显式触发）。
	// 元数据补录（写面：fileRoute 写子组；读面：fileRouteRead 只读子组）。
	// 卷 API（主 mux：fileRoute[Read] = authMiddleware + requireRole，per-owner 文件面）。
	// 只读子组：GET /api/volumes、GET /api/volumes/user（列卷清单）；写子组：
	// move/rebalance/copy、create/delete 用户卷、backends-presign。
	// 卷备份/导出（roadmap 11.3-②）：导出 = 只读子组（reader 可读）；导入 = 写子组。
	// 备份（roadmap 12.2-3 P2）：主 mux 经 authMiddleware（SproxySig/Bearer）——
	// 写面（目标写+配额记账），与 cloud/sync 同模式。
	// 用户卷 API（U3：per-owner 用户自有卷，仅外部类型；fileRoute 认证 + owner 派生）
	// 配额查询**联邦化**（片 5）：只读子组（owner 恒取自认证 ⇒ 不得指定他人 ✗）。
	// backend 列表 API（V4：动态感知已注册后端类型；fileRoute[Read] 认证）
	// secret 卷管理 API（主 mux 面：authMiddleware；写/删走 fileRoute 写子组，导出/列表
	// 走只读子组——与卷 API 同模式。secrets 是 ExternalBackend 不受通用文件 API 覆盖，
	// 专用端点负责创建/列表/导出/删除；服务端只校验+落盘，不参与口令派生）。
	// 计量报告导出（roadmap 11.10-⑩ 片 2）：主 mux 经 authMiddleware（SproxySig/
	// APIKey 认证）——handler 内按 owner 自查询或管理员全量裁决（越权 403）。
	// /api/mesh/acl：owner 过滤**本身就是**边界（actor → owner），且不触碰文件系统，故不挂
	// fileRoute 的角色门禁（与 /api/mesh/status 同款；挂 fileRoute 反而会因不在文件组而 500）。
	// 公网分发端点：独立前缀（/s）、仅主 mux、public 裸注册，**不属分享管理组**
	// （share.RoutePrefix 为 /api），故留在装配层注册。
	srvMux.HandleFunc("GET /s/{token}", h.accessShareHandler)

	// 分享管理 API：路由表由 pkg/server/share 声明（前缀 + 相对路径 + 两端分类）。
	// 主 mux 走文件组角色门（创建/撤销 = 写子组 user，列表 = 只读子组 reader）；
	// 隧道内层三条均 tunnel（隧道加密即认证，裸注册）。
	router.Register(srvMux, localMux, share.RoutePrefix, share.Routes(share.Endpoints{
		Create: h.createShareHandler,
		List:   h.listSharesHandler,
		Revoke: h.revokeShareHandler,
	}), bindHandler, h.routeWrapper)

	// 云任务 CRUD（P1-c S2a）已迁入 pkg/server/cloudtasks：HTTP 面在包内自洽，owner
	// （任务归属与可见性）由 cloudTaskHandler 在装配层从认证上下文解析后显式传入
	// （ActorFrom 的 ctx key 是装配层私有，core 不得持有 ctx→actor 键）。
	cloudTasks := h.cloudTaskService()
	// 通用 URL 解析 API（隧道认证）：URL → 资源信息（大小/类型/文件清单）。
	// 扩展按 URL 自动识别（pikpak 分享等），未识别回落通用结果。

	// 云端下载/归档 API：路由表由 pkg/server/cloudtasks 声明（前缀 + 相对路径 + 两端分类），
	// 这里只提供 handler 绑定与「分类 → 包装器」映射；双 mux 一次注册，不会漏掉另一半。
	router.Register(srvMux, localMux, cloudtasks.RoutePrefix, cloudtasks.Routes(),
		func(fn cloudtasks.Handler) http.HandlerFunc {
			// 方法表达式适配：Service 实例在装配期已固定（单实例语义见 cloudTaskService）。
			return h.cloudTaskHandler(func(w http.ResponseWriter, r *http.Request, owner string) {
				fn(cloudTasks, w, r, owner)
			})
		},
		h.routeWrapper,
	)

	// 文件同步 API：单一声明点见 routes_sync.go（9 条路由 × 2 个 mux → 一份表）。
	// Server=auth（SproxySig 验签）/ Local=tunnel（隧道加密即认证）；syncMgr 未装配时
	// handler 返回 400。
	router.Register(srvMux, localMux, syncRoutePrefix, syncRoutes(),
		func(fn func(*Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { fn(h, w, r) }
		},
		h.routeWrapper)

	// 运维/管理面（审计/集群/配置/统计/用量/mesh/备份/checksum/解析/事件流）：
	// 单一声明点见 routes_ops.go（27 处手工注册 → 一份表 + 一次注册）。
	router.Register(srvMux, localMux, opsRoutePrefix, opsRoutes(),
		func(fn func(*Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { fn(h, w, r) }
		},
		h.routeWrapper)

	// Hub 中继管理 API（需鉴权）：中继流 / WebRTC 信令 / 节点与统计 / 联邦端点，
	// 以及隧道内层用户面裸注册。详细语义见 registerHubRoutes。
	registerHubRoutes(h, srvMux, localMux, opts, cfg, log)

	// 审计查看 API：GET /api/audit 同时注册主 mux（authMiddleware，SproxySig/APIKey
	// 认证）与 localMux（隧道内层，隧道加密即认证——与 /api/shares、/api/stats 同
	// 模式）。审计是浏览器隧道模式下的用户面操作，隧道内层必须可达（用户在隧道
	// 模式下打开审计 tab 应能直接查看；仅注册主 mux 会让隧道模式 404）。
	// 文件变更事件流（主 mux 面）：authMiddleware 保护（直连必须验签）。
	// 审计导出（主 mux 面）：authMiddleware 保护，与 /api/audit 同款（导出是敏感运维
	// 面，直连必须验签；localMux 面裸注册见上方注释）。
	// 冷段区间读（同上；主面经 authMiddleware，管理员门在 handler 内）。

	// 凭据 API：单一声明点见 routes_credentials.go（10 条 × 2 个 mux → 一份表）。
	//
	// 三条公开入口（register/nonce/login）各自独立限流器（login 与 nonce 隔离配额：
	// nonce 是预注册前的低频步骤，登录是高频爆破面）。**限流器必须在注册之前创建**——
	// 此前 localMux 侧先注册、后初始化，方法值捕获 nil 指针 ⇒ 隧道模式下请求 panic
	// 断连（客户端只见 EOF）。guardWrapper 对未初始化限流器 fail-closed panic，把顺序
	// 错误暴露在启动期而不是运行期。
	h.registerLimiter = ratelimit.NewRateLimiter(5, time.Minute, log.With("component", "register_limiter"))
	totpPerMin := 10
	if opts.TotpRateLimit > 0 {
		totpPerMin = opts.TotpRateLimit
	}
	h.totpLimiter = ratelimit.NewRateLimiter(totpPerMin, time.Minute, log.With("component", "totp_limiter"))
	loginPerMin := 10
	if opts.LoginRateLimit > 0 {
		loginPerMin = opts.LoginRateLimit
	}
	h.loginLimiter = ratelimit.NewRateLimiter(loginPerMin, time.Minute, log.With("component", "login_limiter"))
	router.Register(srvMux, localMux, credentialRoutePrefix, credentialRoutes(),
		func(fn func(*Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { fn(h, w, r) }
		},
		h.routeWrapper)
	// 外部认证（OIDC/LDAP，roadmap 11.7-⑥）登录面是**动态**路由（随配置生成），
	// 无法进静态表：主 mux 公开注册 + ipGate，隧道内层裸注册（与凭据登录同模式）。
	for _, extRoute := range h.externalAuthRoutes {
		srvMux.Handle(extRoute.Method+" "+extRoute.Pattern, h.ipGate(extRoute.Handler))
		localMux.Handle(extRoute.Method+" "+extRoute.Pattern, extRoute.Handler)
	}

	srvMux.HandleFunc("GET /livez", h.livez)
	srvMux.HandleFunc("GET /readyz", h.readyz)
	srvMux.HandleFunc("GET /healthz", h.healthz)
	srvMux.HandleFunc("GET /version", h.versionHandler)
	srvMux.Handle("GET /metrics", h.MetricsAuth(http.HandlerFunc(h.MetricsHandler)))
	// 受认证保护 /debug/pprof（roadmap 6.3 P2 内存观测）：DebugPprofEnabled
	// 显式开关默认关；开启才挂载（关闭 = 404 零回归），且经 authMiddleware。
	h.registerPprofRoutes(srvMux)
	// /tunnel 走 authMiddleware：SproxySig 验签成功后按 AK 查 SK 派生隧道密钥
	// （SetTunnelKey 放入 ctx），隧道 handler 用 ctx 密钥解密 metadata/body、加密响应。
	// 未验签的请求 401；隧道内层 localMux 请求（解密后转发）由隧道加密本身提供认证。
	srvMux.Handle("POST /tunnel", h.authMiddleware(http.HandlerFunc(h.tunnelHandler.ServeHTTP)))

	// 本地卷 WebDAV 挂载面（roadmap P1 服务端 WebDAV）：/dav/ 经 authMiddleware
	// 认证（SproxySig/APIKey），按 owner 解析租户 user 桶根 → webdav.NewHandler。
	// 任意 WebDAV 客户端（curl/文件管理器/rsync）直接读写本服务存储（owner 隔离）。
	// webdav.enabled=false（缺省）不装配（零回归）；true 时装配。
	if cfg.WebDAV.Enabled {
		log.Info("WEBDAV-REGISTER", "enabled", cfg.WebDAV.Enabled)
		srvMux.Handle("/dav/", h.authMiddleware(http.HandlerFunc(h.davService().Handler)))
	} else {
		log.Info("WEBDAV-SKIP", "enabled", cfg.WebDAV.Enabled)
	}

	// S3 兼容服务端（roadmap P2）：/s3/<key> 经 SigV4 验签（Authorization
	// AWS4-HMAC-SHA256，AK = sproxy 凭据 → ring 查 SK 验签）→ owner 卷。
	srvMux.Handle("/s3/", http.HandlerFunc(h.s3Service().Handler))

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

	// 文件面 srvMux 也走 gzip（roadmap P2 残余：Accept-Encoding 协商 + Content-Type
	// 白名单自动压缩；apiHandler 已有，srvMux 的文件下载/列表响应同样受益）。
	// 集群写面门（roadmap 12.1-2）：writeGuard 非 nil 时**外层 srvMux 同样受门**
	// （cloud/config/sync/credentials 等非文件组写路由在此层）——apiHandler 只覆盖
	// 隧道内层，外层必须独立挂 writeGuardMiddleware。
	// 链序（由外到内）：metrics → requestLog → gzip → **auditVolume**（先把卷维度绑进 ctx，
	// 保证内层审计中间件与业务 handler 都能读到）→ audit（传输兜底）→ writeGuard → 路由。
	h.handler = h.metricsMiddleware(h.requestLogMiddleware(GzipMiddleware(log.With("component", "gzip"))(
		h.auditVolumeMiddleware(h.auditMiddleware(h.writeGuardMiddleware(srvMux, srvMux))))))

	return h
}

// matchedPattern 返回请求在 next mux 上的**已匹配模式**（形如 "POST /upload"）。
//
// 为什么不能直接用 r.Pattern：包住 mux 的中间件在 mux 匹配**之前**执行，此时
// r.Pattern 仍为空（它由 ServeMux 在调用 handler 时写入）。`mux.Handler(r)` 只做
// 匹配、不执行 handler，因此能在中间件层拿到同一个模式串——查表的键与注册键同构。
// 非 mux（测试替身/自定义 handler）回落 r.Pattern（可能为空 = 视为未声明）。
func matchedPattern(mux *http.ServeMux, r *http.Request) string {
	if mux != nil {
		if _, pattern := mux.Handler(r); pattern != "" {
			return pattern
		}
	}
	return r.Pattern
}

// authorizeTunnelFileClass 按声明分类对隧道内层文件域请求收口角色门禁。
// 返回 false 表示已写回错误响应（调用方直接返回）。
//
// principal 为 nil（xfer 直连，无外层身份）时放行——xfer 会话由握手密钥 /
// Ed25519 pinning 闭合身份（见 localMuxGate 的说明）。
func authorizeTunnelFileClass(w http.ResponseWriter, r *http.Request, class router.Class) bool {
	p := PrincipalFrom(r.Context())
	if p == nil {
		return true
	}
	minRole := string(accesskey.RoleUser)
	if class == router.ClassReader {
		minRole = string(accesskey.RoleReader)
	}
	if err := requireRole(p, minRole); err != nil {
		status := http.StatusForbidden
		if errors.Is(err, errUnauthorized) {
			status = http.StatusUnauthorized
		}
		http.Error(w, err.Error(), status)
		return false
	}
	return true
}

// localMuxGate 包装隧道内层 localMux（含传统 POST /tunnel 与 xfer 直连两路径共用的
// apiHandler 链）：对「文件域路由」按**声明分类**收口角色门禁。
//
// 分类来源是 `route_class_index.go` 的查表（键 = ServeMux 已匹配模式 `r.Pattern`），
// 不再用请求路径重新推导字符串白名单——那套写法曾把「模式串 vs 具体路径」的歧义变成
// **静默越权**（漏列 ⇒ 判为非文件组 ⇒ 完全不做角色检查）。
func (h *Handlers) localMuxGate(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class, declared := fileDomainClass(matchedPattern(mux, r))
		if !declared || (class != router.ClassUser && class != router.ClassReader) {
			// 非文件域（cloud/credentials/audit/stats/config/hub/信令…）：
			// 保持既有裸注册（隧道加密即认证 / 免身份语义）。
			next.ServeHTTP(w, r)
			return
		}
		// 文件域：principal 非 nil（传统隧道路径外层已认证）才收口；nil（xfer
		// 直连，无外层身份）跳过——xfer 会话由握手密钥 / Ed25519 pinning 闭合身份。
		// 前提：HubXferKey 候选必须是 user/admin 凭据（node AK 不应进入 xfer 握手
		// 密钥候选集——若 node 凭据排序在前，xfer 面会对该 node 开放文件面），见
		// task-3-report.md「隧道内层门禁收口说明」。
		if !authorizeTunnelFileClass(w, r, class) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeGuardMiddleware 是集群写面门（roadmap 12.1-2 只读副本接入）：writeGuard 非 nil
// 时，写面路由前置 Authorize()——非主节点（replica/follower）ErrNotLeader → 503；nil =
// 单节点零回归（写面全放行）。判定口诀（设计 cluster-readonly-replica.md §2.2）：路由
// 语义「改状态（文件/meta/配置）」→ 设门；「只读状态」→ 不设门。
//
// 文件写子组按**声明分类**判定（表驱动，与 fileRoute 同源）；非文件组写面见
// isWriteFaceRoute 的清单（PUT /api/config、凭据/cloud/sync/volumes 写族、notify test、
// ai purge、verify、S3 写）。
func (h *Handlers) writeGuardMiddleware(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.writeGuard == nil {
			next.ServeHTTP(w, r)
			return
		}
		if !isWriteFaceRoute(r.Method, matchedPattern(mux, r)) {
			next.ServeHTTP(w, r)
			return
		}
		if err := h.writeGuard.Authorize(); err != nil {
			if errors.Is(err, leader.ErrNotLeader) {
				http.Error(w, "cluster: 当前节点非写面主节点（只读副本）", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isWriteFaceRoute 判定路由是否属于写面（集群只读副本的写门）。
//
// 判据来自**声明表**（`mutatingPatterns`，见 route_class_index.go）：文件域/分享由
// ClassUser 派生，其余域在表里显式 `Mutating: true`。不再维护第二份路径清单——
// 那套清单漏改的后果是只读副本上仍可写入（副本与主分叉）。
//
// 特例：S3 兼容网关是 `/s3/` 子树挂载（不是一条 Route 声明），其写方法在此显式判定。
//
// pattern 传 ServeMux 已匹配模式（`mux.Handler(r)`）；兼容测试传入的纯路径（自动补方法前缀）。
func isWriteFaceRoute(method, pattern string) bool {
	full := pattern
	if !strings.HasPrefix(full, method+" ") {
		full = method + " " + pattern
	}
	if mut, ok := declaredMutating(full); ok {
		return mut
	}
	// S3 子树挂载：非 GET/HEAD 即写（P1：原先不在写面集合，副本可经 /s3/ 写入）。
	path := strings.TrimPrefix(full, method+" ")
	if strings.HasPrefix(path, "/s3/") && method != http.MethodGet && method != http.MethodHead {
		return true
	}
	return false
}

// fileRoute 包装文件操作**写**子组路由：authMiddleware 认证 + requireRole(user)
// 门禁（DEC-C，写面 zero-regression——与 RBAC 细分前完全一致）。认证面插件化后，
// 文件操作路由组要求 Role∈{user,admin}（minRole=user；R3-M4：空 Role 归一 user
// 放行）。未认证且非回环直通（principal==nil）→ 401；角色不足 → 403。
//
// 分类判定 = `route_class_index.go` 的查表（键为 `r.Pattern`）：未声明或分类不是
// ClassUser 一律 500 + Error 日志（fail-closed）——防止包装器被误用于非文件写子组，
// 或分类声明被改错却静默降低鉴权。非文件组路由继续用 authMiddleware。
func (h *Handlers) fileRoute(handler http.HandlerFunc) http.HandlerFunc {
	return h.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if class, ok := fileDomainClass(r.Pattern); !ok || class != router.ClassUser {
			// fail-closed：本包装被错误用于非文件写子组路径（如未来把 cloud 路由误挂
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

// fileRouteRead 是只读子组路由包装（RBAC 细分 11.5-①）：authMiddleware +
// requireRole(reader) 门禁（Role∈{reader,user,admin}）。与 fileRoute 仅差 minRole
// 与「非只读路径 fail-closed」；写路由保持 fileRoute（user/admin，零回归）。
func (h *Handlers) fileRouteRead(handler http.HandlerFunc) http.HandlerFunc {
	return h.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if class, ok := fileDomainClass(r.Pattern); !ok || class != router.ClassReader {
			h.logger.Error("fileRouteRead 用于非文件只读子组的路径（接线错误）", "method", r.Method, "path", r.URL.Path)
			http.Error(w, "internal: route not in file group", http.StatusInternalServerError)
			return
		}
		if err := requireRole(PrincipalFrom(r.Context()), string(accesskey.RoleReader)); err != nil {
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

// bindHandler 是「H 已是装配层 http.HandlerFunc」的路由组的 bind 参数（恒等映射）。
// 与 cloudtasks 的方法表达式不同，这些包的 handler 全在装配层（含 ctx→actor 闭包），
// 故经 Endpoints 绑定后无需再转换。
func bindHandler(fn http.HandlerFunc) http.HandlerFunc { return fn }

// guardWrapper 把 Guard 记号映射为**附加入口控制**（限流器 [+ 主 mux 的 IP 门]）。
//
// Guard 与 Class 的分工：Class 决定基础鉴权（auth/user/reader/public/tunnel），
// Guard 决定同一分类内的额外策略（如三个公开凭据入口各自的限流器）。
// 未知记号或未初始化的限流器**一律 panic**（fail-closed：不允许静默裸放公开入口）。
func (h *Handlers) guardWrapper(c router.Class, guard string) func(http.HandlerFunc) http.HandlerFunc {
	var limiter *ratelimit.RateLimiter
	switch guard {
	case guardCredentialRegister:
		limiter = h.registerLimiter
	case guardCredentialNonce:
		limiter = h.totpLimiter
	case guardCredentialLogin:
		limiter = h.loginLimiter
	default:
		panic("router: 未知 Guard " + guard + "（未在装配层定义）")
	}
	if limiter == nil {
		// 装配顺序错误（先注册后初始化）会让方法值捕获 nil 指针，请求期 panic 断连
		// ——隧道模式的三条公开凭据入口曾因此完全不可用。
		panic("router: Guard " + guard + " 的限流器未初始化（注册必须在限流器创建之后）")
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		mw := limiter.Middleware(next)
		if c == router.ClassPublic {
			// 白名单部署要求所有公开入口一致：主 mux 面额外挂 IP 门。
			mw = h.ipGate(mw)
		}
		return mw.ServeHTTP
	}
}

// routeWrapper 把路由分类映射为装配层的包装器（供 router.Register 使用）。
//
// **分类 → 包装策略的唯一定义处**：子包只声明分类，不能自带包装器，因此「是否鉴权、
// 走哪条角色门」的安全决定始终留在装配层。未列出的分类直接 panic（fail-closed）——
// 新增分类必须显式决定其包装策略，不允许静默裸注册。
func (h *Handlers) routeWrapper(c router.Class, guard string) func(http.HandlerFunc) http.HandlerFunc {
	if guard != "" {
		return h.guardWrapper(c, guard)
	}
	switch c {
	case router.ClassAuth:
		return h.authMiddleware
	case router.ClassUser:
		return h.fileRoute
	case router.ClassReader:
		return h.fileRouteRead
	case router.ClassPublic:
		return nil // 公开端裸注册（handler 内自带门禁，如 feed_token / 公开订阅源）
	case router.ClassTunnel:
		return nil // 隧道内层裸注册（隧道加密即认证，与既有 localMux 语义一致）
	}
	panic("router: 分类 " + string(c) + " 未在装配层定义包装策略")
}
