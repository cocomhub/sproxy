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
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/authn"
	"github.com/cocomhub/sproxy/pkg/leader"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/telemetry"
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
	// XferMetrics 是传输层扩展指标提供者（装配层注入；nil = 不输出 WS/QUIC
	// 指标行）。cmd/sproxy 经 go.work import ext/ws、ext/quic 提供——pkg/server
	// 不直接依赖独立 module（web/e2e 等子 module 经 pkg/server 间接编译不破）。
	XferMetrics XferMetricsProvider
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
	auditRing, auditStore := setupAuditServices(cfg, log)

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
		rebalanceProg: newRebalanceProgress(),
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
		auditStore:    auditStore,
		notifyCenter:  newNotifyCenterFromConfig(cfg.Notify, log),
		alertEngine:   newAlertEngineFromConfig(cfg.Alerts, log),
		// per-AK 失败锁定表（U4）：恒装配（登录端点存在即需；上限 + 惰性清理见
		// loginFailTracker 注释）。
		loginFailTracker: newLoginFailTracker(),
		totpPending:      newTotpPendingTable(),
		// 测试注入空 Ring 时的无认证调试兜底（一次性读取；生产走 cfg.AllowInsecureLoopback）。
		allowInsecureLoopback: opts.AllowInsecureLoopback,
		volSet:                vs,
		xferMetrics:           opts.XferMetrics,
		writeGuard:            opts.WriteGuard,
	}
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

	// 本地路由子 mux（无 authMiddleware，隧道密钥已提供认证）
	localMux := http.NewServeMux()
	localMux.HandleFunc("POST /upload", h.upload)
	localMux.HandleFunc("GET /download", h.download)
	localMux.HandleFunc("POST /delete", h.delete)
	localMux.HandleFunc("POST /rename", h.rename)
	localMux.HandleFunc("GET /api/files", h.listFiles)
	localMux.HandleFunc("HEAD /api/files/stat", h.stat)
	localMux.HandleFunc("GET /api/du", h.duHandler)
	localMux.HandleFunc("POST /mkdir", h.mkdir)
	localMux.HandleFunc("POST /rmdir", h.rmdir)
	localMux.HandleFunc("GET /api/files/search", h.searchFiles)
	localMux.HandleFunc("GET /api/search/semantic", h.semanticSearchHandler)
	localMux.HandleFunc("POST /api/tags", h.tagsHandler)
	localMux.HandleFunc("POST /api/batch/delete", h.batchDelete)
	localMux.HandleFunc("POST /api/batch/rename", h.batchRename)

	localMux.HandleFunc("POST /api/archive", h.archiveHandler)
	localMux.HandleFunc("GET /api/archive-dir", h.archiveDirHandler)
	localMux.HandleFunc("GET /api/versions", h.listVersionsHandler)
	localMux.HandleFunc("POST /api/versions/restore", h.restoreVersionHandler)
	localMux.HandleFunc("DELETE /api/versions", h.deleteVersionHandler)
	// 回收站（roadmap P2）：列表/恢复/清空（隧道内层裸注册同版本管理）。
	localMux.HandleFunc("GET /api/trash", h.listTrashHandler)
	localMux.HandleFunc("POST /api/trash/restore", h.restoreTrashHandler)
	localMux.HandleFunc("POST /api/trash/empty", h.emptyTrashHandler)
	// 卷 API（隧道内层裸注册：隧道加密即认证，与版本/share 同模式）
	localMux.HandleFunc("GET /api/volumes", h.listVolumesHandler)
	localMux.HandleFunc("POST /api/volumes/move", h.moveVolumeHandler)
	localMux.HandleFunc("POST /api/volumes/rebalance", h.rebalanceVolumeHandler)
	localMux.HandleFunc("POST /api/volumes/copy", h.copyVolumeHandler)
	// 卷备份/导出（roadmap 11.3-②）：导出 = 只读（fileRouteRead / 只读子组）；
	// 导入 = 写（fileRoute / 写子组）。
	localMux.HandleFunc("GET /api/volumes/export", h.exportVolumeHandler)
	localMux.HandleFunc("POST /api/volumes/import", h.importVolumeHandler)
	// 备份（roadmap 12.2-3 P2）：源=本地卷 → 目标=配置卷（federated 写面）；
	// 隧道内层裸注册（隧道加密即认证，与卷 API 同模式）。
	localMux.HandleFunc("POST /api/backup", h.backupHandler)
	// 用户卷 API（隧道内层裸注册：与系统卷同模式；CLI --access-key 走此路径）
	localMux.HandleFunc("POST /api/volumes/user", h.createUserVolumeHandler)
	localMux.HandleFunc("GET /api/volumes/user", h.listUserVolumesHandler)
	localMux.HandleFunc("DELETE /api/volumes/user", h.deleteUserVolumeHandler)
	localMux.HandleFunc("GET /api/stats", h.statsHandler)
	localMux.HandleFunc("GET /api/config", h.configHandler)
	// backend 列表 API（隧道内层裸注册：CLI --access-key 走此路径；供 Web/CLI 动态感知后端）
	localMux.HandleFunc("GET /api/backends", h.backendsHandler)
	localMux.HandleFunc("POST /api/backends/{type}/presign", h.backendPresignHandler)
	localMux.HandleFunc("POST /api/backends/{type}/presign/complete", h.backendPresignCompleteHandler)
	// 跨节点面只读运维视图（隧道内层：加密即认证，与 /api/config 同模式）。
	localMux.HandleFunc("GET /api/mesh/status", h.meshStatusHandler)
	localMux.HandleFunc("GET /api/mesh/acl", h.meshACLHandler)
	localMux.HandleFunc("PUT /api/config", h.updateConfigHandler)
	// 审计查看：隧道内层注册（无 authMiddleware——隧道加密即认证，与 /api/shares、
	// /api/stats 的 localMux 侧同模式）。auditHandler 只读 ring 回 JSON，自身不做
	// 签名校验。浏览器隧道模式下用户面操作必须隧道可达（仅注册主 mux 会 404）。
	localMux.HandleFunc("GET /api/audit", h.auditHandler)
	// 计量报告导出（roadmap 11.10-⑩ 片 2）：隧道内层裸注册（隧道加密即认证，与
	// /api/stats 同模式）；handler 内做 owner 自查询/管理员全量权限裁决。
	localMux.HandleFunc("GET /api/usage/report", h.usageReportHandler)
	// 文件变更事件流（roadmap §2 P1）：SSE 订阅 upload/delete/rename/mkdir/rmdir/version。
	// 隧道内层裸注册（隧道加密即认证，与 audit/share 同模式）；外层经 authMiddleware 保护。
	localMux.HandleFunc("GET /api/events", h.eventsHandler)
	localMux.HandleFunc("GET /api/audit/export", h.auditExportHandler)
	// 通知中心运维（roadmap P0）：历史查看 + 渠道自检（隧道内层裸注册同 audit 模式）。
	// RSS/Atom 订阅端点（roadmap 11.7-⑦）：feed 是订阅源——localMux 裸注册
	// （隧道加密即认证）；主 mux 侧另注册公开面（feed_token 门禁在 handler 内，
	// 无 authMiddleware，仿 /metrics 语义）。
	localMux.HandleFunc("GET /api/notify/history", h.notifyHistoryHandler)
	localMux.HandleFunc("POST /api/notify/test", h.notifyTestHandler)
	localMux.HandleFunc("GET /api/notify/feed", h.notifyFeedHandler)
	// AI 文件洞察端点（roadmap 11.9-⑤；未装配（ai.insight.enabled=false）→ 400 零回归）。
	localMux.HandleFunc("POST /api/ai/summarize", func(w http.ResponseWriter, r *http.Request) {
		h.handleAISummarize(w, r, ownerFromRequest(r), h.aiInsight)
	})
	localMux.HandleFunc("POST /api/ai/tag", func(w http.ResponseWriter, r *http.Request) {
		h.handleAITag(w, r, ownerFromRequest(r), h.aiInsight)
	})
	localMux.HandleFunc("GET /api/ai/quota", h.handleAIQuota)
	localMux.HandleFunc("GET /api/ai/privacy", h.handleAIPrivacy)
	localMux.HandleFunc("POST /api/ai/privacy/purge", h.handleAIPrivacyPurge)
	localMux.HandleFunc("GET /api/cluster/nodes", h.handleClusterNodes)
	localMux.HandleFunc("GET /api/cluster/self", h.handleClusterSelf)
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
	// 外部认证（OIDC/LDAP，roadmap 11.7-⑥）登录面：localMux 侧裸注册（浏览器隧道
	// 模式下登录页可达，与凭据登录端点同模式）。未配置（externalAuthRoutes 空）→
	// 零回归（无外部端点）。
	for _, extRoute := range h.externalAuthRoutes {
		localMux.Handle(extRoute.Method+" "+extRoute.Pattern, extRoute.Handler)
	}
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

	// 隧道内层中间件链（gzip + 速率限制 + CORS + localMuxGate 角色门禁 +
	// writeGuard 写面门 + requestLog 追踪）与 tunnelHandler/localHandler 装配。
	// 详细语义见 buildTunnelHandlers。
	h.buildTunnelHandlers(localMux, cfg, log)

	// 文件操作路由组（DEC-C）：authMiddleware + requireRole 门禁
	// （upload/download/delete/rename/list/stat/mkdir/rmdir/search/batch/chunk/
	// archive/versions/share…）。RBAC 细分（11.5-①）：只读子组
	// （isReadOnlyFileRoute）走 fileRouteRead = requireRole(reader)
	// （Role∈{reader,user,admin}）；写子组保持 fileRoute = requireRole(user)
	// （Role∈{user,admin}，一字不改零回归）。
	// 全仓 checksum 巡检（roadmap 11.3-⑩）：POST /api/verify 是运维审计面，走
	// authMiddleware（SproxySig 验签）即可——与 /api/stats 同模式，不做文件组门禁
	// （巡检读全卷台账+重算，非 per-file 用户面）。
	localMux.HandleFunc("POST /api/verify", h.verifyHandler)
	srvMux.HandleFunc("POST /api/verify", h.authMiddleware(h.verifyHandler))
	srvMux.HandleFunc("POST /upload", h.fileRoute(h.upload))
	srvMux.HandleFunc("GET /download", h.fileRouteRead(h.download))
	srvMux.HandleFunc("POST /delete", h.fileRoute(h.delete))
	srvMux.HandleFunc("POST /rename", h.fileRoute(h.rename))
	srvMux.HandleFunc("GET /api/files", h.fileRouteRead(h.listFiles))
	srvMux.HandleFunc("HEAD /api/files/stat", h.fileRouteRead(h.stat))
	srvMux.HandleFunc("GET /api/du", h.fileRouteRead(h.duHandler))
	srvMux.HandleFunc("POST /upload/init", h.fileRoute(h.uploadInit))
	srvMux.HandleFunc("POST /upload/chunk", h.fileRoute(h.uploadChunk))
	srvMux.HandleFunc("GET /upload/status", h.fileRoute(h.uploadStatus))
	srvMux.HandleFunc("GET /upload/sessions", h.fileRoute(h.uploadSessions))
	srvMux.HandleFunc("POST /upload/complete", h.fileRoute(h.uploadComplete))
	srvMux.HandleFunc("GET /download/chunk", h.fileRouteRead(h.downloadChunk))
	srvMux.HandleFunc("POST /mkdir", h.fileRoute(h.mkdir))
	srvMux.HandleFunc("POST /rmdir", h.fileRoute(h.rmdir))
	srvMux.HandleFunc("GET /api/files/search", h.fileRouteRead(h.searchFiles))
	srvMux.HandleFunc("GET /api/search/semantic", h.fileRouteRead(h.semanticSearchHandler))
	srvMux.HandleFunc("GET /api/ai/quota", h.handleAIQuota)
	srvMux.HandleFunc("GET /api/ai/privacy", h.handleAIPrivacy)
	srvMux.HandleFunc("POST /api/ai/privacy/purge", h.handleAIPrivacyPurge)
	srvMux.HandleFunc("GET /api/cluster/nodes", h.handleClusterNodes)
	srvMux.HandleFunc("GET /api/cluster/self", h.handleClusterSelf)
	srvMux.HandleFunc("POST /api/tags", h.fileRoute(h.tagsHandler))
	srvMux.HandleFunc("POST /api/batch/delete", h.fileRoute(h.batchDelete))
	srvMux.HandleFunc("POST /api/batch/rename", h.fileRoute(h.batchRename))
	srvMux.HandleFunc("POST /api/archive", h.fileRoute(h.archiveHandler))
	srvMux.HandleFunc("GET /api/archive-dir", h.fileRouteRead(h.archiveDirHandler))
	srvMux.HandleFunc("GET /api/versions", h.fileRouteRead(h.listVersionsHandler))
	srvMux.HandleFunc("POST /api/versions/restore", h.fileRoute(h.restoreVersionHandler))
	srvMux.HandleFunc("DELETE /api/versions", h.fileRoute(h.deleteVersionHandler))
	srvMux.HandleFunc("GET /api/trash", h.authMiddleware(h.listTrashHandler))
	srvMux.HandleFunc("POST /api/trash/restore", h.authMiddleware(h.restoreTrashHandler))
	srvMux.HandleFunc("POST /api/trash/empty", h.authMiddleware(h.emptyTrashHandler))
	// 卷 API（主 mux：fileRoute[Read] = authMiddleware + requireRole，per-owner 文件面）。
	// 只读子组：GET /api/volumes、GET /api/volumes/user（列卷清单）；写子组：
	// move/rebalance/copy、create/delete 用户卷、backends-presign。
	srvMux.HandleFunc("GET /api/volumes", h.fileRouteRead(h.listVolumesHandler))
	srvMux.HandleFunc("POST /api/volumes/move", h.fileRoute(h.moveVolumeHandler))
	srvMux.HandleFunc("POST /api/volumes/rebalance", h.fileRoute(h.rebalanceVolumeHandler))
	srvMux.HandleFunc("POST /api/volumes/copy", h.fileRoute(h.copyVolumeHandler))
	// 卷备份/导出（roadmap 11.3-②）：导出 = 只读子组（reader 可读）；导入 = 写子组。
	srvMux.HandleFunc("GET /api/volumes/export", h.fileRouteRead(h.exportVolumeHandler))
	srvMux.HandleFunc("POST /api/volumes/import", h.fileRoute(h.importVolumeHandler))
	// 备份（roadmap 12.2-3 P2）：主 mux 经 authMiddleware（SproxySig/Bearer）——
	// 写面（目标写+配额记账），与 cloud/sync 同模式。
	srvMux.HandleFunc("POST /api/backup", h.authMiddleware(h.backupHandler))
	// 用户卷 API（U3：per-owner 用户自有卷，仅外部类型；fileRoute 认证 + owner 派生）
	srvMux.HandleFunc("POST /api/volumes/user", h.fileRoute(h.createUserVolumeHandler))
	srvMux.HandleFunc("GET /api/volumes/user", h.fileRouteRead(h.listUserVolumesHandler))
	srvMux.HandleFunc("DELETE /api/volumes/user", h.fileRoute(h.deleteUserVolumeHandler))
	// backend 列表 API（V4：动态感知已注册后端类型；fileRoute[Read] 认证）
	srvMux.HandleFunc("GET /api/backends", h.fileRouteRead(h.backendsHandler))
	srvMux.HandleFunc("POST /api/backends/{type}/presign", h.fileRoute(h.backendPresignHandler))
	srvMux.HandleFunc("POST /api/backends/{type}/presign/complete", h.fileRoute(h.backendPresignCompleteHandler))
	srvMux.HandleFunc("GET /api/stats", h.authMiddleware(h.statsHandler))
	srvMux.HandleFunc("GET /api/config", h.authMiddleware(h.configHandler))
	// 计量报告导出（roadmap 11.10-⑩ 片 2）：主 mux 经 authMiddleware（SproxySig/
	// APIKey 认证）——handler 内按 owner 自查询或管理员全量裁决（越权 403）。
	srvMux.HandleFunc("GET /api/usage/report", h.authMiddleware(h.usageReportHandler))
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

	// 分享管理 API（主 mux：Bearer auth + requireRole 门禁）。只读子组：
	// GET /api/shares（列表，reader 可读）；写子组：DELETE /api/shares/{token}
	// （撤销，至少 user）。
	srvMux.HandleFunc("GET /api/shares", h.fileRouteRead(h.listSharesHandler))
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
	localMux.HandleFunc("POST /api/sync/tasks/{id}/retry", h.syncRetryTask)
	localMux.HandleFunc("DELETE /api/sync/tasks/{id}", h.syncDeleteTask)
	localMux.HandleFunc("GET /api/sync/conflicts", h.syncListConflicts)
	localMux.HandleFunc("GET /api/sync/conflicts/{id}", h.syncGetConflict)
	localMux.HandleFunc("POST /api/sync/conflicts/{id}/resolve", h.syncResolveConflict)
	// 文件同步 API（主 mux：SproxySig auth）
	srvMux.HandleFunc("POST /api/sync/tasks", h.authMiddleware(h.syncCreateTask))
	srvMux.HandleFunc("GET /api/sync/tasks", h.authMiddleware(h.syncListTasks))
	srvMux.HandleFunc("GET /api/sync/tasks/{id}", h.authMiddleware(h.syncGetTask))
	srvMux.HandleFunc("POST /api/sync/tasks/{id}/cancel", h.authMiddleware(h.syncCancelTask))
	srvMux.HandleFunc("POST /api/sync/tasks/{id}/retry", h.authMiddleware(h.syncRetryTask))
	srvMux.HandleFunc("DELETE /api/sync/tasks/{id}", h.authMiddleware(h.syncDeleteTask))
	srvMux.HandleFunc("GET /api/sync/conflicts", h.authMiddleware(h.syncListConflicts))
	srvMux.HandleFunc("GET /api/sync/conflicts/{id}", h.authMiddleware(h.syncGetConflict))
	srvMux.HandleFunc("POST /api/sync/conflicts/{id}/resolve", h.authMiddleware(h.syncResolveConflict))

	// Hub 中继管理 API（需鉴权）：中继流 / WebRTC 信令 / 节点与统计 / 联邦端点，
	// 以及隧道内层用户面裸注册。详细语义见 registerHubRoutes。
	registerHubRoutes(h, srvMux, localMux, opts, cfg, log)

	// 审计查看 API：GET /api/audit 同时注册主 mux（authMiddleware，SproxySig/APIKey
	// 认证）与 localMux（隧道内层，隧道加密即认证——与 /api/shares、/api/stats 同
	// 模式）。审计是浏览器隧道模式下的用户面操作，隧道内层必须可达（用户在隧道
	// 模式下打开审计 tab 应能直接查看；仅注册主 mux 会让隧道模式 404）。
	srvMux.HandleFunc("GET /api/audit", h.authMiddleware(h.auditHandler))
	// 文件变更事件流（主 mux 面）：authMiddleware 保护（直连必须验签）。
	srvMux.HandleFunc("GET /api/events", h.authMiddleware(h.eventsHandler))
	// 审计导出（主 mux 面）：authMiddleware 保护，与 /api/audit 同款（导出是敏感运维
	// 面，直连必须验签；localMux 面裸注册见上方注释）。
	srvMux.HandleFunc("GET /api/audit/export", h.authMiddleware(h.auditExportHandler))
	srvMux.HandleFunc("GET /api/notify/history", h.authMiddleware(h.notifyHistoryHandler))
	srvMux.HandleFunc("POST /api/notify/test", h.authMiddleware(h.notifyTestHandler))
	// 通知订阅端点（roadmap 11.7-⑦）：公开面（feed 是订阅源，阅读器抓取不带业务
	// 凭据）——feed_token 门禁在 handler 内（空=公开；非空=?token/Bearer，常量时间
	// 比较），不挂 authMiddleware（仿 /metrics 语义；独立于 SproxySig/APIKey）。
	srvMux.HandleFunc("GET /api/notify/feed", h.notifyFeedHandler)

	// 公开注册端点（4B DEC-F）：唯一用户入口，不挂 authMiddleware（主 mux +
	// localMux 双注册，仿 /healthz 层）——仅经独立限频 registerLimiter 收口。
	// register 激活前系统处于零凭据态，无凭据请求必须可直达（远程首注册仅经回环
	// 门禁拒绝，见 registerCredentialHandler）。
	//
	// registerLimiter 必须在 localMux 装配之前、localMux 侧复用同一限频器实例创建。
	h.registerLimiter = NewRateLimiter(5, time.Minute, log.With("component", "register_limiter"))
	registerPublic := h.registerLimiter.Middleware(http.HandlerFunc(h.registerCredentialHandler))
	// 公开凭据端点（register/nonce/login）同挂 IP 门（设计文档 ip-whitelist §4）：
	// 白名单部署要求所有入口一致——公开端点不经 authMiddleware，单独包 ipGate。
	srvMux.Handle("POST /api/credentials/register", h.ipGate(registerPublic))

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
	srvMux.Handle("POST /api/credentials/nonce", h.ipGate(h.totpLimiter.Middleware(http.HandlerFunc(h.nonceHandler))))

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
	srvMux.Handle("POST /api/credentials/login", h.ipGate(h.loginLimiter.Middleware(http.HandlerFunc(h.loginCredentialHandler))))

	// 外部认证（OIDC/LDAP，roadmap 11.7-⑥）登录面：主 mux 公开注册（不挂
	// authMiddleware——登录是免认证入口；经 ipGate 一致收口公开端点）。
	// 未配置（externalAuthRoutes 空）→ 零回归（无外部端点）。
	for _, extRoute := range h.externalAuthRoutes {
		srvMux.Handle(extRoute.Method+" "+extRoute.Pattern, h.ipGate(extRoute.Handler))
	}

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
		srvMux.Handle("/dav/", h.authMiddleware(http.HandlerFunc(h.davHandler)))
	} else {
		log.Info("WEBDAV-SKIP", "enabled", cfg.WebDAV.Enabled)
	}

	// S3 兼容服务端（roadmap P2）：/s3/<key> 经 SigV4 验签（Authorization
	// AWS4-HMAC-SHA256，AK = sproxy 凭据 → ring 查 SK 验签）→ owner 卷。
	srvMux.Handle("/s3/", http.HandlerFunc(h.s3Handler))

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
	h.handler = h.metricsMiddleware(h.requestLogMiddleware(GzipMiddleware(log.With("component", "gzip"))(h.writeGuardMiddleware(srvMux))))

	return h
}

// isFileGroupedRoute 判定给定路由是否属于「文件操作组」（单一事实源，拒绝双份清单
// 漂移，F1/F2 收口）。文件操作组的成员 = 主 mux 面经 fileRoute[Read] 包装的路由
// 全集：
//   - 精确匹配：upload/download/delete/rename、api/files*/mkdir/rmdir/batch*、
//     archive/versions/share/shares（share 列表 /api/shares 与撤销已含）；
//   - 前缀分支：/upload/{init,chunk,status,sessions,complete} 与 /download/chunk
//     （分块上传/下载，与 fileRoute 包裹的 chunk 组严格对齐）。
//
// 用途：
//   - fileRoute / fileRouteRead（主 mux 面）：path 命中组 → 按只读/写子组分别过
//     requireRole(reader/user)；未命中 → 返回 errNotFileGrouped，调用方写 500 +
//     Error 日志（fail-closed 防接线错误把文件类新路由漏挂门禁——宁可显式故障，
//     不为未列出的新文件路由静默放行）；
//   - localMuxGate（隧道内层面）：path 命中组 → principal 非 nil（传统 POST
//     /tunnel 路径）时按只读/写子组 requireRole(reader/user) 收口、node → 403；
//     principal nil（xfer 直连路径）跳过（会话由握手密钥/pinning 闭合）；未命中 →
//     保持既有裸注册（隧道加密即认证 / cloud/credentials/audit/stats/config/hub
//     等非文件面）。
//
// 新增文件类路由必须同步：既挂主 mux fileRoute[Read]，又在本函数与
// isReadOnlyFileRoute 补成员——三处同源，漏其一即测试
// （TestRegister_TunnelInnerGate* / TestLocalMuxCoversAllTunnelRoutes /
// TestRBAC_ReadOnlyRouteClassification）暴露。
func isFileGroupedRoute(path string) bool {
	switch path {
	case "/upload", "/download", "/delete", "/rename",
		"/api/files", "/api/files/stat", "/api/files/search", "/api/search/semantic", "/api/tags", "/api/du",
		"/mkdir", "/rmdir", "/api/batch/delete", "/api/batch/rename",
		"/api/archive", "/api/archive-dir",
		"/api/versions", "/api/versions/restore",
		routeVolumesBase, "/api/volumes/move", "/api/volumes/rebalance", "/api/volumes/copy", "/api/volumes/user",
		"/api/volumes/export", "/api/volumes/import",
		"/api/backends",
		"/api/backends/{type}/presign",
		"/api/backends/{type}/presign/complete",
		"/api/share", "/api/shares",
		// 分块上传/下载（主 mux 面均挂 fileRoute[Read]——见 RegisterRoutes 装配处清单）；
		// 前缀含两个入口：/upload/{init,chunk,status,sessions,complete}。
		"/upload/init", "/upload/chunk", "/upload/status", "/upload/sessions", "/upload/complete",
		"/download/chunk":
		return true
	}
	// 动态参数路径组（Go 1.22 ServeMux {token} 通配——调用方传入的是实际 path，
	// 需按前缀判定）：/api/shares/{token}（撤销也属文件组）。精确列表 /api/shares
	// 已在上方案例命中；此处补带 token 子路径。
	return strings.HasPrefix(path, "/api/shares/")
}

// isReadOnlyFileRoute 判定文件组内路由是否属于「只读子组」（RBAC 细分 11.5-①
// 单一事实源，与 isFileGroupedRoute 同文件同风格）。只读子组 = reader 账号可访问
// 的文件组路由（GET/list/search/stat/download/share 列表/卷列表等）；其余文件组
// 成员即写子组（requireRole(user) 一字不改，零回归）。
//
// 成员（对齐设计文档 docs/designs/2026-09-24-rbac-roles.md §3）：
//   - GET /download、GET /api/files、HEAD /api/files/stat、GET /api/files/search
//     （主读面）；
//   - GET /download/chunk（分块下载，读面）；
//   - GET /api/versions（版本历史只读；versions-restore 是写子组）；
//   - GET /api/archive-dir（可存档目录列表）；
//   - GET /api/backends（后端列表）；
//   - GET /api/volumes、GET /api/volumes/user（卷清单只读）；
//   - GET /api/volumes/export（卷备份导出，只读）；
//   - GET /api/shares 与 /api/shares/{token} 前缀组（分享列表；DELETE 撤销是写子组）。
//
// 用途：fileRouteRead（主 mux 面）与 localMuxGate（隧道内层面）同源：命中只读
// 子组 → requireRole(reader)；命中写子组 → requireRole(user)。漏列/误列由
// TestRBAC_ReadOnlyRouteClassification 与隧道读写双测兜住（设计文档风险 1）。
// isReadOnlyFileRoute 判定文件组内路由是否只读子组（RBAC 细分 11.5-①）：
// method 区分（GET/HEAD = 只读；其余 = 写）。单一事实源，与 isFileGroupedRoute
// 同文件同风格。命中只读子组 → requireRole(reader)；写子组 → requireRole(user)。
func isReadOnlyFileRoute(path, method string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false // DELETE /api/versions（删版本）等写面一律走 user 门禁
	}
	switch path {
	case "/download", "/api/files", "/api/files/stat", "/api/files/search", "/api/du",
		"/download/chunk", "/api/versions", "/api/archive-dir", "/api/backends",
		routeVolumesBase, "/api/volumes/user", "/api/volumes/export", "/api/shares":
		return true
	}
	return strings.HasPrefix(path, "/api/shares/")
}

// localMuxGate 包装隧道内层 localMux（含传统 POST /tunnel 与 xfer 直连两路径共用的
// apiHandler 链）：对「文件操作路由组」过 requireRole(PrincipalFrom(ctx)) 门禁
// （任务② MUST-FIX 收口 + RBAC 细分：只读子组 requireRole(reader)、写子组
// requireRole(user)）。组成员与子组判定 = isFileGroupedRoute /
// isReadOnlyFileRoute（与主 mux fileRoute[Read] 同源）。判定语义见 isFileGroupedRoute
// / RegisterRoutes 装配处大段注释。
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
			minRole := string(accesskey.RoleUser)
			if isReadOnlyFileRoute(r.URL.Path, r.Method) {
				minRole = string(accesskey.RoleReader)
			}
			if err := requireRole(p, minRole); err != nil {
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

// writeGuardMiddleware 是集群写面门（roadmap 12.1-2 只读副本接入）：writeGuard 非 nil
// 时，写面路由前置 Authorize()——非主节点（replica/follower）ErrNotLeader → 503；nil =
// 单节点零回归（写面全放行）。判定口诀（设计 cluster-readonly-replica.md §2.2）：路由
// 语义「改状态（文件/meta/配置）」→ 设门；「只读状态」→ 不设门。
//
// 写面路径集合（与设计 §2.2 写面清单对齐）：
//   - 文件写子组（isFileGroupedRoute && !isReadOnlyFileRoute：upload/delete/rename/
//     mkdir/rmdir/batch/archive/versions-restore/trash-restore-empty/tags）；
//   - 非文件组写面：/api/config（PUT）、/api/credentials（写族）、/api/cloud/*
//     （任务创建/取消/删除）、/api/sync/tasks（写族）、/api/volumes（写族）、
//     /api/notify/test、/api/ai/privacy/purge、/api/verify。
//
// 读面（下载/列表/搜索/统计/健康）不设门。
func (h *Handlers) writeGuardMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.writeGuard == nil {
			next.ServeHTTP(w, r)
			return
		}
		if !isWriteFaceRoute(r.URL.Path, r.Method) {
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

// isWriteFaceRoute 判定路径是否属于写面（设计 §2.2 写面清单 + 文件写子组）。
func isWriteFaceRoute(path, method string) bool {
	// 文件写子组：文件组 && 非只读子组（与 fileRoute 判定同源）。
	if isFileGroupedRoute(path) && !isReadOnlyFileRoute(path, method) {
		return true
	}
	// 非文件组写面：PUT /api/config、凭据写族、cloud 任务写族、sync 任务写族、
	// volumes 写族、notify test、ai purge、verify。
	switch {
	case path == "/api/config" && method == http.MethodPut:
		return true
	case path == "/api/verify":
		return true
	case path == "/api/backup":
		return true
	case strings.HasPrefix(path, "/api/credentials") && method != http.MethodGet:
		return true
	case strings.HasPrefix(path, "/api/cloud/") && method != http.MethodGet:
		return true
	case strings.HasPrefix(path, "/api/sync/tasks") && method != http.MethodGet:
		return true
	case strings.HasPrefix(path, routeVolumesBase) && method != http.MethodGet:
		return true
	case path == "/api/notify/test":
		return true
	case path == "/api/ai/privacy/purge":
		return true
	}
	return false
}

// fileRoute 包装文件操作**写**子组路由：authMiddleware 认证 + requireRole(user)
// 门禁（DEC-C，写面 zero-regression——与 RBAC 细分前完全一致）。认证面插件化后，
// 文件操作路由组要求 Role∈{user,admin}（minRole=user；R3-M4：空 Role 归一 user
// 放行）。未认证且非回环直通（principal==nil）→ 401；角色不足 → 403。
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
		if isReadOnlyFileRoute(r.URL.Path, r.Method) {
			// 接线错误：写包装包了只读子组（应走 fileRouteRead）——fail-closed 500，
			// 防「reader 读被误按写面拦截」静默出现（设计文档错误处理最后一条）。
			h.logger.Error("fileRoute 用于只读子组路径（接线错误，应走 fileRouteRead）", "method", r.Method, "path", r.URL.Path)
			http.Error(w, "internal: route in read-only subgroup", http.StatusInternalServerError)
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
		if !isFileGroupedRoute(r.URL.Path) {
			h.logger.Error("fileRouteRead 用于未列入文件组的路径（接线错误）", "method", r.Method, "path", r.URL.Path)
			http.Error(w, "internal: route not in file group", http.StatusInternalServerError)
			return
		}
		if !isReadOnlyFileRoute(r.URL.Path, r.Method) {
			// 接线错误：只读包装包了写子组（应走 fileRoute）——fail-closed 500。
			h.logger.Error("fileRouteRead 用于写子组路径（接线错误，应走 fileRoute）", "method", r.Method, "path", r.URL.Path)
			http.Error(w, "internal: route not in read-only subgroup", http.StatusInternalServerError)
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
