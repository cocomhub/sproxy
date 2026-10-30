// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// routes_setup.go 是 RegisterRoutes 的装配辅助：把路由注册函数按逻辑段落拆成
// 独立 helper（审计/存储布局/分享持久化/认证链/调度器/存储管理器/隧道中间件链/
// hub 路由），降低 RegisterRoutes 的认知复杂度（go:S3776）。每个 helper 只做
// 单一装配段落，行为与抽取前逐字等价。

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"

	"sync"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/authn"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/integrity"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// setupAuditServices 装配审计服务：有界内存环形缓冲 + 落盘存储。
//
// 有界内存环形审计缓冲：按 cfg.Audit.BufferSize 装配（默认 2048 由 SetDefaults
// 保证；显式 0 = 关闭，auditRing 保持 nil，GET /api/audit 返回空表）。
// 审计落盘（roadmap §2 P1）：默认启用，落盘到 <默认卷根>/audit/audit.log
// （合适位置自动选择，无需配置目录）。打开失败记错误并降级为 ring-only
// （审计绝不阻断启动）；装载失败同样降级。
// 轮转（roadmap 11.5-⑥）：audit.max_size > 0 时按大小轮转 + 保留 max_archives 份归档。
func setupAuditServices(cfg *Config, coldFS func() syncpkg.FS, am audit.Metrics, log *slog.Logger) (*audit.Service, *audit.FileJournal) {
	if am == nil {
		am = audit.NullMetrics{} // 兜底：未注入时保持零回归（但生产装配应注入 auditMetrics）
	}
	if cfg.Audit.Disabled {
		// 显式关闭（audit.disabled=true）：不建目录、不落盘、读面空表（审计完全 off）。
		log.Warn("审计已显式关闭（audit.disabled=true）")
		return nil, nil
	}
	// 显式 `buffer_size: 0` = **只关内存环形索引**（读面改由落盘段提供；落盘仍按下方装配）。
	// 评审 R13 P1：此前在这里直接 `return nil, nil` ⇒ journal/service 全被丢弃 = 整条审计链
	// 被静默关闭（连文件都不建），与 config.go / docs/config.md 的说明**相反** ✗。
	indexEnabled := cfg.Audit.BufferSize > 0
	root := resolveDefaultVolumeRoot(cfg)
	// 热段目录 = <卷根>/__audit__/hot/：`__` 前缀被 ListOwners 跳过 ⇒ 不污染租户命名空间
	// （旧位置 <卷根>/audit/ 会被当租户枚举且可被注册占用 —— 见设计 §5.6 点 5）。
	dir := filepath.Join(root, "__audit__", "hot")
	migrateLegacyAuditDir(filepath.Join(root, "audit"), dir, log)

	var journal *audit.FileJournal
	// 封段回调（M4）：轮转产生封段 ⇒ 立即归档（在 Journal 锁外触发，见 pkg/audit 契约）。
	onSeal, bindJournal := newSealArchiveHook(cfg, coldFS, log)
	j, err := audit.NewFileJournal(audit.FileJournalConfig{
		Dir:           dir,
		Layout:        audit.LayoutGlobal, // 合规段族：全局时间线（便于合规导出/检索）
		GlobalName:    "audit.log",        // 沿用既有段文件名（与历史迁移同源，便于运维识别）
		SyncMode:      audit.SyncBatch,    // 策略族默认：批量 + 阈值 flush（设计 §5.2）
		MaxSize:       int64(cfg.Audit.MaxSize),
		MaxArchives:   cfg.Audit.MaxArchives,
		MaxTotalBytes: int64(cfg.Audit.MaxTotalBytes),
		OnSeal:        onSeal,
		Metrics:       am, // 丢弃/写失败/轮转可观测（评审 R13 P1）
		Logger:        log,
	})
	if err != nil {
		// 启动期行为可配（设计 §5.6 点 6 / M1-14）：默认**降级**（审计绝不阻断启动）；
		// 显式 fail_fast=true（合规部署要求审计必须可用）⇒ 中止启动，避免「以为有审计、
		// 实际没落盘」的静默失效 —— 这是审计最危险的失败形态（设计 §5.4.2 同旨）。
		if cfg.Audit.Startup.FailFast {
			panic("audit: 落盘装配失败且 audit.startup.fail_fast=true（配置要求审计必须可用）: " + err.Error())
		}
		log.Error("审计落盘装配失败（降级为仅内存；如需中止启动请设 audit.startup.fail_fast=true）",
			"error", err.Error())
	} else {
		journal = j
		bindJournal(j) // 供封段回调使用（此后轮转即触发归档）
	}

	// M4 冷段归档：启动期这一次由**装配层**在卷集合就绪后触发（见 RegisterRoutes），
	// 因为归档目的地可能是**卷**（需解析卷 FS）；轮转后的归档由 OnSeal 回调触发 ✓。

	caps := audit.FullCapabilities()
	var jr audit.Journal = audit.NullJournal{}
	if journal != nil {
		jr = journal
	}
	var idx audit.Index = audit.NullIndex{}
	if indexEnabled {
		idx = audit.NewRingIndex(cfg.Audit.BufferSize)
	}
	svc, serr := audit.New(audit.Options{
		Journal: jr,
		Index:   idx,
		Logger:  log,
		Metrics: am,
		Policy:  audit.Policy{MaxMemoryEntries: cfg.Audit.BufferSize},
		Caps:    &caps,
		CtxFields: audit.ContextFields{
			Actor: ActorFrom,
			Owner: ActorFrom, // 租户键 = AK（设计 §5.7 规则 1；严禁 Principal.Owner）
			Mesh:  MeshFrom,
			// 请求维度（评审 R13 P2）：此前未接线 ⇒ 条目里恒为空，而设计把它们列为敏感字段。
			RequestID: auditRequestIDFrom,
			SourceIP:  auditSourceIPFrom,
			// 卷维度（评审 R13）：与其余 5 维同构 ⇒ 业务调用点无需逐条传。
			Volume: auditVolumeFrom,
		},
	})
	if serr != nil {
		if cfg.Audit.Startup.FailFast {
			panic("audit: 服务装配失败且 audit.startup.fail_fast=true: " + serr.Error())
		}
		log.Error("审计服务装配失败（审计关闭）", "error", serr.Error())
		return nil, journal
	}
	return svc, journal
}

// migrateLegacyAuditDir 把旧位置 <卷根>/audit/ 的审计段一次性迁入新位置（best-effort）。
// 不做静默丢弃：迁移失败只记日志（旧文件保留在原处，不影响启动）。
func migrateLegacyAuditDir(legacyDir, newDir string, log *slog.Logger) {
	des, err := os.ReadDir(legacyDir)
	if err != nil {
		return // 不存在或不可读：无历史可迁
	}
	if merr := os.MkdirAll(newDir, 0o700); merr != nil {
		log.Warn("审计热段目录创建失败", "dir", newDir, "error", merr.Error())
		return
	}
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, "audit.log") && !strings.Contains(name, "audit.log.") {
			continue
		}
		from := filepath.Join(legacyDir, name)
		to := filepath.Join(newDir, name)
		if _, serr := os.Stat(to); serr == nil {
			continue // 目标已存在：不覆盖
		}
		if rerr := os.Rename(from, to); rerr != nil {
			log.Warn("审计历史迁移失败（保留原处）", "from", from, "error", rerr.Error())
			continue
		}
		log.Info("审计历史已迁移到新热段目录", "from", from, "to", to)
	}
}

// initStorageLayout 装配多租户存储布局：默认卷租户缓存 + 全局配额池 + 预创建
// anonymous 租户，并装配告警引擎（磁盘水位轮询 / 配额预警）。
//
// 默认卷租户缓存（h.tenants）在 globalRoot 赋值之后构造——它绑定该根；checksumStores/
// uploadStores/quotaScopes 仍为懒创建（首次请求时建），见各 *For 辅助。
// globalRoot 语义 = 默认卷根（F1 裁决；单卷形态 = cfg.StorageRoot，既有 tenantFor/handler
// 默认卷语义零回归）；globalPool 仍是 owner 全局 max 兜底（cfg.MaxStorageBytes，跨卷合计）。
func (h *Handlers) initStorageLayout(vs *registry.Set, cfg *Config, log *slog.Logger) {
	h.globalRoot = vs.DefaultRoot()
	h.globalPool = quota.NewPool(cfg.MaxStorageBytes)
	// 运维余量接线（独立审核 P2，2026-10-18）：`Pool.SetOperationalReserve` 此前**零生产
	// 调用点** ⇒ `p.reserve` 恒 0 ⇒ usableLocked 不扣余量 ⇒ 用户裁定的「绝不磁盘打满」
	// 从未生效。此处按配置接入（0 = 不保留，零回归）。
	h.globalPool.SetOperationalReserve(cfg.OperationalReserveBytes)
	// 默认卷租户：预建 meta 桶（per-tenant checksum / 凭据记录落点）。非默认卷由
	// pkg/volume/registry 按卷各持一个缓存（不预建 meta，见 Set.Tenant）。
	h.tenants = storage.NewTenantCache(h.globalRoot,
		storage.WithMetaBucket(), storage.WithLogger(h.logger))
	h.checksumStores = make(map[string]checksum.ChecksumStoreIface)
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
	// 告警引擎装配：把通知中心渠道并入告警引擎（规则 channels 名共用），
	// 并启动磁盘水位轮询（cfg.Alerts.Enabled 时）。
	if h.alertEngine != nil {
		h.alertEngine.AdoptNotifyChannels(h.notifyCenter)
		// 告警根因建议器（roadmap 11.9-⑥）：notify.ai_advisor.enabled + key 解析成功才装配；
		// 未启用/无 key → NewAIAdvisor 内部 gate=nil（恒空，fail-closed 回退模板）并在此 Warn 一次。
		if advisor := newAIAdvisorFromConfig(cfg.Notify.AIAdvisor, log); advisor != nil {
			h.alertEngine.SetAdvisor(advisor)
		}
		h.alertEngine.SetDiskUsageReader(func() (used, capVal int64) {
			p := h.globalPool
			if p == nil {
				return 0, 0
			}
			return p.Usage(), p.MaxBytes()
		})
		// 配额预警（roadmap P2）：per-owner 水位轮询（owner_quotas 达阈值 → 通知）。
		h.alertEngine.SetOwnerList(h.SyncTenantList())
		h.alertEngine.SetQuotaUsageReader(func(owner string) (used, capVal int64) {
			// 🔴 **修复（2026-10-16）**：此前用 `h.quotaScopeFor(owner, "")` —— 而 `rel == ""` 会被
			// 判为「非功能桶首段」并**恒返回 nil** ✗ ⇒ 读取器**永远得到 (0,0)** ⇒
			// **per-owner 配额预警静默失效** ✗（无日志/指标/报错 ✗）。
			// 改用 `ownerQuotaScope`（owner 根 Scope）—— 其 `committed` 经 `commitUp` 沿父链累计
			// ⇒ **就是该 owner 的全子树用量** ✓（**不是**全局池用量 ✗：每个树节点本身是一个
			// `Pool`，`Scope.Usage()` 返回 `p.committed` ✓），`MaxBytes()` = 该 owner 的配额上限 ✓。
			sc := h.ownerQuotaScope(owner)
			if sc == nil {
				return 0, 0
			}
			return sc.Usage(), sc.MaxBytes()
		})
		h.alertEngine.Start()
	}
}

// shareRel 是分享链接持久化在 anonymous 租户卷内的相对目录
// （<默认卷根>/anonymous/meta/share）。
const shareRel = "meta/share"

// initSharePersist 装配分享链接持久化（§10-③）：分享是服务级资源（token 全局唯一、
// 跨租户可访问），落盘到 anonymous 租户 meta/share（<默认卷根>/anonymous/meta/share/）。
// 启用后 Create/Consume/Revoke/过期清理同步原子写/删 <token>.json，重启恢复未过期链接。
// 依赖 anonymous 租户已预建（initStorageLayout 的 tenantFor(anonymousOwner) 检查
// 通过 ⇒ meta 桶存在）。集群模式（statestore.md §5.2）切 StateStore 后端——逐 token
// key share/<token> + Consume 计数 CAS（一次性/限量防超发）；旧 meta 仅回退读。
func (h *Handlers) initSharePersist(opts RegisterRoutesOpts, cfg *Config, log *slog.Logger) {
	if tnt := h.tenantFor(anonymousOwner); tnt != nil && tnt.Root() != nil {
		// 经 anonymous 租户**卷句柄** + "meta/share" 相对目录落盘：卷自己决定是否加密
		// （使用方不理解加密态），并天然继承封印判定。
		if opts.StateStore != nil {
			h.SetShareStore(newStateBackedShareStore(opts.StateStore, tnt.Root(), shareRel, log))
		} else {
			h.shareStore.EnablePersist(tnt.Root(), shareRel)
		}
	}
}

// initAuthAndRestore 装配凭据自动轮换、认证链（DEC-C）、外部认证登录面、信令收件箱
// 恢复与路由表快照持久化回调。依赖 bootstrapCredentials 已装配 credentialRing。
func (h *Handlers) initAuthAndRestore(opts RegisterRoutesOpts, cfg *Config, log *slog.Logger) {
	// 凭据自动轮换周期 goroutine（credentials.rotation.interval > 0 时启动；0 = 关闭，
	// 零回归）。依赖 bootstrapCredentials 已装配 credentialRing；Close() 关 rotationStop。
	h.initCredentialRotation(cfg)
	// 认证链装配（DEC-C）+ 外部认证（OIDC/LDAP）登录面。详细语义见 initAuthChain。
	h.initAuthChain(opts, log)
	// 信令收件箱恢复 + 路由表快照持久化回调。详细语义见 restoreSignalAndSnapshot。
	h.restoreSignalAndSnapshot(opts)
}

// initCredentialRotation 启动凭据自动轮换周期 goroutine（interval > 0 时）。
func (h *Handlers) initCredentialRotation(cfg *Config) {
	if rc := rotationConfigFromCfg(cfg); rc.interval > 0 {
		h.rotationStop = make(chan struct{})
		h.rotationWg.Go(func() {
			h.credentialRotationLoop()
		})
	}
}

// initAuthChain 装配认证链（DEC-C）：宿主注入的 Authenticators 非 nil → replace 默认链
// （宿主全权掌控，需含 RingAuthenticator 则自行加入，R3-I2）。**显式注入空链（非 nil
// 空切片）同样尊重**——空链 = 无任何 authenticator → 所有请求未认证（authMiddleware
// 走 handleNoCredentials 兜底，不被默认链覆盖）；nil（未注入）→ 默认
// [RingAuthenticator{credentialRing}]（R3-I1：4A 默认行为零回归）。
func (h *Handlers) initAuthChain(opts RegisterRoutesOpts, log *slog.Logger) {
	if opts.Authenticators != nil {
		h.authenticators = opts.Authenticators
	} else {
		h.authenticators = []Authenticator{NewRingAuthenticator(h.credentialRing, h.noncePool,
			WithRingLogger(log), WithURLSigTTL(h.urlSigTTLFromCfg()))}
	}
	// 外部认证（OIDC/LDAP，roadmap 11.7-⑥）登录面 + 认证链成员：装配层注入的
	// ExternalAuthHandlers 追加到链尾（外部 Bearer/会话 cookie 与 SproxySig 互斥，
	// 先后无冲突；设计文档数据流 3）。未配置（nil）= 零回归。
	if len(opts.ExternalAuthHandlers) > 0 {
		h.appendExternalAuthRoutes(opts)
	}
}

// appendExternalAuthRoutes 外部认证（OIDC/LDAP）登录面路由入表 + 认证器入链（追加到
// 认证链尾部；外部 Bearer/会话 cookie 与 SproxySig 互斥，先后无冲突）。
func (h *Handlers) appendExternalAuthRoutes(opts RegisterRoutesOpts) {
	for _, ext := range opts.ExternalAuthHandlers {
		if ext == nil {
			continue
		}
		for _, rt := range ext.Routes() {
			if rt.Method == "" || rt.Pattern == "" || rt.Handler == nil {
				continue
			}
			h.externalAuthRoutes = append(h.externalAuthRoutes, rt)
		}
		// 认证器入链：Provider.Authenticators() 返回 OIDC/LDAP 子认证器。
		if ap, ok := ext.(interface{ Authenticators() []authn.Authenticator }); ok {
			h.authenticators = append(h.authenticators, ap.Authenticators()...)
		}
	}
}

// trashGCTask 构造回收站周期 GC 任务（trash.gc_interval > 0 时注册）：TTL 每 tick
// 现读（热更新可见）；间隔保留现逻辑（>0 用配置值，否则默认 1h）。
func (h *Handlers) trashGCTask(cfg *Config) Task {
	return Task{
		Name:            "trash-gc",
		Interval:        cfg.Trash.GCInterval,
		MaintenanceOnly: true,
		Run: func(context.Context) {
			ttl := cfg.Trash.TTL
			if ttl <= 0 {
				ttl = 7 * 24 * time.Hour
			}
			for _, owner := range h.SyncTenantList()() {
				// all=true：**各卷** trash 都要按 TTL 过期（软删落文件所在卷；此前只清默认卷
				// ⇒ 其它卷的条目永不过期，记档缺陷 2026-10-19 ③）。
				_, _ = h.fileService().CleanupTrashIn(context.Background(), owner, "", true, ttl)
			}
		},
	}
}

// verifyPeriodicTask 构造全仓 checksum 巡检周期任务（verify_interval > 0 时注册）：
// 单飞防重入（busy 时跳过本 tick，不堆叠）；结果写审计 + 告警联动（verifyPass 内
// RecordAudit，与手动端点同款）。
func (h *Handlers) verifyPeriodicTask(cfg *Config) Task {
	return Task{
		Name:            "checksum-verify",
		Interval:        cfg.VerifyInterval,
		MaintenanceOnly: true,
		Run: func(ctx context.Context) {
			rep := h.verifyOnce(ctx, "", "")
			detail := fmt.Sprintf("periodic ok=%d mismatched=%d missing=%d errors=%d skipped=%d",
				rep.Ok, len(rep.Mismatched), len(rep.Missing), len(rep.Errors), rep.Skipped)
			result := AuditResultSuccess
			if len(rep.Mismatched) > 0 || len(rep.Missing) > 0 || len(rep.Errors) > 0 {
				result = AuditResultError
			}
			h.RecordAudit(ctx, AuditEvent{
				Action: "verify", ObjectType: "volume", Object: "periodic",
				Result: result, Detail: detail,
			})
			if len(rep.Mismatched)+len(rep.Missing) > 0 && h.alertEngine != nil {
				h.alertEngine.OnChecksumMismatch(ctx, "", len(rep.Mismatched)+len(rep.Missing), detail)
			}
		},
	}
}

// restoreSignalAndSnapshot 启动时恢复持久化的信令收件箱，并装配路由表变更持久化回调。
func (h *Handlers) restoreSignalAndSnapshot(opts RegisterRoutesOpts) {
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
}

// initSchedulerTasks 装配统一任务调度器与各周期 goroutine：uploading 清理 /
// share-cleanup / trash-gc / version-gc / checksum-verify 周期任务 + 计量报告 +
// 搜索索引快照 + 卷镜像 + 冷热分层 + 卷级保留期清理。
//
// 统一任务调度器（roadmap 11.10-H2）：四个既有周期 GC 循环收敛。间隔 1:1 保留
// （upload 10m / share 5m / trash gc_interval 默认 1h / version gc_interval）；
// MaintenanceOnly = version-gc/trash-gc（维护窗口外跳过）；share-cleanup FinalRunOnStop
// （退出前补清一次，保留 ShareStore 旧语义）；维护窗口默认关闭（inWindow=nil 恒执行，零回归）。
func (h *Handlers) initSchedulerTasks(cfg *Config, log *slog.Logger) {
	// 统一任务调度器（roadmap 11.10-H2）与各周期任务注册。详细语义见 registerPeriodicTasks。
	h.registerPeriodicTasks(cfg, log)
	// 其余周期 goroutine：搜索索引快照 / 卷镜像 / 冷热分层 / 卷级保留期清理。
	h.startPeriodicLoops(cfg)
}

// registerPeriodicTasks 装配统一任务调度器与周期任务：uploading 清理 / share-cleanup /
// trash-gc / version-gc / checksum-verify + 计量报告。
//
// 统一任务调度器（roadmap 11.10-H2）：四个既有周期 GC 循环收敛。间隔 1:1 保留
// （upload 10m / share 5m / trash gc_interval 默认 1h / version gc_interval）；
// MaintenanceOnly = version-gc/trash-gc（维护窗口外跳过）；share-cleanup FinalRunOnStop
// （退出前补清一次，保留 ShareStore 旧语义）；维护窗口默认关闭（inWindow=nil 恒执行，零回归）。
func (h *Handlers) registerPeriodicTasks(cfg *Config, log *slog.Logger) {
	if h.scheduler == nil {
		h.scheduler = NewScheduler(log.With("component", "scheduler"))
	}
	h.scheduler.SetMaintenanceWindow(config.ParseMaintenanceWindow(cfg.Scheduler))
	// Register 失败仅可能由编程错误（重名/间隔<=0/缺 Run）触发——注册即 fatal：
	// 任务配置是静态装配，静默跳过会掩盖装配 bug（设计文档：装配层 Error 日志后跳过）。
	mustRegister := func(t Task) {
		if regErr := h.scheduler.Register(t); regErr != nil {
			log.Error("scheduler 任务注册失败，跳过", "task", t.Name, "error", regErr.Error())
		}
	}
	mustRegister(Task{
		Name:     "uploading-cleanup",
		Interval: 10 * time.Minute,
		Run: func(context.Context) {
			h.cleanupUploadingFilesPass()
		},
	})
	mustRegister(Task{
		Name:           "share-cleanup",
		Interval:       5 * time.Minute,
		FinalRunOnStop: true,
		Run: func(context.Context) {
			h.shareStore.cleanupExpired()
		},
	})
	// 回收站周期 GC goroutine（trash.gc_interval > 0 时启动；0 = 关闭，零回归）。
	// 间隔保留现逻辑：>0 用配置值，否则默认 1h；TTL 每 tick 现读（热更新可见）。
	if cfg.Trash.GCInterval > 0 {
		mustRegister(h.trashGCTask(cfg))
	}
	// 版本 GC 周期 goroutine（versioning.gc_interval > 0 时启动；0 = 关闭，零回归）。
	if cfg.Versioning.GCInterval > 0 {
		mustRegister(Task{
			Name:            "version-gc",
			Interval:        cfg.Versioning.GCInterval,
			MaintenanceOnly: true,
			Run: func(context.Context) {
				h.gcAllExpiredVersionsPass()
			},
		})
	}
	// 全仓 checksum 巡检周期任务（verify_interval > 0 时注册；0 = 关闭，零回归）。
	// 复用统一任务调度器（#574）：单飞防重入（busy 时跳过本 tick，不堆叠）；
	// 结果写审计 + 告警联动（verifyPass 内 RecordAudit，与手动端点同款）。
	if cfg.VerifyInterval > 0 {
		mustRegister(h.verifyPeriodicTask(cfg))
	}
	h.scheduler.Start()
	// 计量报告装配（roadmap 11.10-⑩ 片 2）：usage.enabled=true 时懒建 usageStore + 周期
	// 落盘（scheduler 已 Start，故在此注册后需 Start 一次——见 setupUsageReport 内注释）。
	h.setupUsageReport(cfg, log)
	if h.scheduler != nil {
		h.scheduler.Start()
	}
}

// startPeriodicLoops 启动其余周期 goroutine：搜索索引快照 / 卷镜像 / 冷热分层 /
// 卷级保留期清理（各配置 >0 时启动；0 = 关闭，零回归）。
func (h *Handlers) startPeriodicLoops(cfg *Config) {
	h.startIndexSaveLoop(cfg)
	h.startMirrorLoop(cfg)
	h.startTierLoop(cfg)
	h.startRetentionLoop(cfg)
}

// startIndexSaveLoop 启动搜索索引快照周期保存 goroutine（index_save_interval > 0 时；
// 0 = 关闭，零回归）。与 mirror 同构（ticker + stop channel + WaitGroup）；启动时先保存一次。
func (h *Handlers) startIndexSaveLoop(cfg *Config) {
	if cfg.IndexSaveInterval > 0 {
		h.indexSaveStop = make(chan struct{})
		h.indexSaveWg.Go(func() {
			h.indexSaveLoop(cfg.IndexSaveInterval)
		})
	}
}

// startMirrorLoop 启动卷镜像周期 goroutine（mirror_interval > 0 且任一卷配了 mirror_to；
// 0 = 关闭，零回归）。与 versionGC 同构（ticker + stop channel + WaitGroup）。
func (h *Handlers) startMirrorLoop(cfg *Config) {
	if cfg.MirrorInterval > 0 && h.hasMirrorStrategy() {
		h.mirrorStop = make(chan struct{})
		h.mirrorWg.Go(func() {
			h.mirrorVolumeLoop()
		})
	}
}

// startTierLoop 启动冷热分层自动降级周期 goroutine（tier_policy.interval > 0 时；0 = 关闭，
// 零回归）。与 mirror 同构（ticker + stop channel + WaitGroup）；扫描复用 rebalance 迁移核心。
func (h *Handlers) startTierLoop(cfg *Config) {
	if cfg.TierPolicy.Interval > 0 {
		tm := newTierManager(h, cfg.TierPolicy.Interval)
		h.tierStop = make(chan struct{})
		h.tierWg.Go(func() {
			tm.run(context.Background())
		})
		h.tierWg.Go(func() {
			// 停止信号转发（tierManager.run 监听 ctx 或 stop；此处监听 h.tierStop）。
			<-h.tierStop
			tm.Close()
		})
	}
}

// startRetentionLoop 启动卷级保留期清理周期 goroutine（volumes[].retention.gc_interval > 0；
// 全零 = 关闭，零回归）。与 mirror 同构（ticker + stop channel + WaitGroup）；pass 遍历全部
// 启用卷，按各卷 retention 清理版本/分享/审计（roadmap 11.7-⑨）。
func (h *Handlers) startRetentionLoop(cfg *Config) {
	if !h.hasVolumeRetentionLoop() {
		return
	}
	interval := h.volumeRetentionInterval()
	if interval > 0 {
		h.retentionStop = make(chan struct{})
		h.retentionWg.Go(func() {
			h.volumeRetentionLoop(interval)
		})
	}
}

// volumeRetentionInterval 返回全部启用卷 retention.gc_interval 的最小间隔（最早 tick）。
func (h *Handlers) volumeRetentionInterval() time.Duration {
	interval := time.Duration(0)
	for _, v := range h.volSet.All() {
		if v.Retention.GCInterval > 0 {
			if interval == 0 || v.Retention.GCInterval < interval {
				interval = v.Retention.GCInterval
			}
		}
	}
	return interval
}

// initStorageManagers 初始化 StorageManager 和 CloudDownloadManager。
// P4：StorageManager 保留全局账本（sync/旧装配兼容）；启动扫描经 SetReconciler 按租户桶
// 归集校准 per-tenant 配额 Scope（重启后 Scope 不回溯）。云任务配额走 cloud 桶子 Scope。
// 多卷（任务 3）：StorageManager 扫描目录 = 默认卷根（vs.Default().RootDir——单一事实源，
// 与 assembleVolumes 的 i==0 裁决共用同一装配产物，防两处裁决漂移；单卷形态 = cfg.StorageRoot，
// 零回归）；reconcile 双目标——owner 全局 Scope（reconcileQuotaScopes）+ 默认卷容量池校准
// （reconcileVolumePool）。多卷逐卷扫描校准框架见 reconcileVolumes（T4 与写路径一并接线）。
func (h *Handlers) initStorageManagers(vs *registry.Set, cfg *Config, log *slog.Logger, opts RegisterRoutesOpts) {
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
		SyncThreshold:       cfg.CloudSyncThreshold,
		MaxConcurrent:       cfg.CloudMaxConcurrent,
		TransferConcurrency: cfg.CloudTransferConcurrency,
		MaxBatchURLs:        cfg.CloudMaxBatchURLs,
		TaskTTL:             cfg.CloudTaskTTL,
		FailedTaskTTL:       cfg.CloudFailedTaskTTL,
		AllowPrivate:        cfg.CloudDownloadAllowPrivate,
		DownloadTimeout:     cfg.CloudDownloadTimeout,
		IdleTimeout:         cfg.CloudDownloadIdleTimeout,
		MaxRetries:          cfg.CloudMaxRetries,
		RetryDelay:          cfg.CloudRetryDelay,
		Downloader:          cfg.CloudDownloader,
		MaxCheckMemBytes:    cfg.CloudCheckMemBytes,
		// P2-7 修复：trusted_volume.skip_verify 接线到转存读侧校验（此前 cloudCfg 缺该
		// 字段，transfer.go 的 m.config.SkipVerify 恒 false——配置项与文档不符）。
		SkipVerify: cfg.TrustedVolume.SkipVerify,
	}
	// 云端下载经 mesh 出口：由装配层（cmd/sproxy）构造 CloudExitDial 注入
	// （pkg/server 不 import pkg/client——client 测试 import server 构成包级环，
	// 装配逻辑放 main 包，复用 newMeshHubClient + mesh.NewLocalOrExitDial）。
	if opts.CloudExitDial != nil {
		cloudCfg.ExitDial = opts.CloudExitDial
	}
	// M3：云任务的启动恢复**唯一触发点** = 统一恢复协调器（见 routes.go 的 setupRecovery）。
	// 构造期不恢复（DeferRecovery）⇒ 子系统不再各自「扫自己的目录」。
	h.cloudMgr = cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{
		DeferRecovery: true,
		UploadsDir:    vs.Default().RootDir, Storage: cloudStorageManager{m: sm},
		TenantFor: h.tenantFor, ChecksumStoreFor: h.checksumStoreFor,
		ListTenants: h.listTenantIDs, Logger: log.With("component", "cloud"),
		Config: cloudCfg,
		QuotaFor: []cloud.QuotaResolver{func(owner string) *quota.Scope {
			return h.quotaBucketFor(owner, "cloud")
		}},
		// 本地 staging 配额钩子（用户裁定 2026-10-10）：外部卷转存写本地暂存统一过
		// 独立 staging Scope（防本地磁盘打满）——cloud 层在 transferDone 按 owner 注入
		// targetFS（Exempt 跳过 / Capable 自管 / 默认 gate 强制）。
		StagingQuotaFor: func(owner string) syncpkg.StagingQuotaTracker {
			return h.stagingQuotaTrackerFor(owner)
		},
		// 转存写目标 rel 复用**文件级写锁池**（与 HTTP 上传/删除/改名/DAV/S3 同一键空间）：
		// 转存不经 pkg/files 写路径，无此锁时与其它写面同 rel 并发写会产出「主文件与
		// sidecar 错位」的固化状态（远端卷写后不回读交叉校验 → 读侧逐分块校验恒失配、
		// 永久 fail-closed）。抢不到 → 可重试目标卷异常，编排层退避重试错开窗口。
		TargetWriteLock: func(owner, rel string) (func(), bool) {
			// M2（第 7 轮 MAJOR）：转存传的是卷 FS 键（共享卷含 `<owner>/` 前缀），
			// 经 transferWriteLock 归一为域侧锁键后再取锁——否则与 HTTP 上传/删除/move
			// 的锁互不可见，同 rel 并发写会产出「主文件与 sidecar 错位」固化状态。
			return h.transferWriteLock(owner, rel)
		},
		// 转存目标卷解析：**唯一外部卷写视图出口** externalVolumeTransferFS（可信卷：
		// Guard(Wrap) 恒包——meta 恒生成 + meta 桶隔离），cloud 下载转存与统一转存共用，
		// 防「一处可信卷、一处裸卷」的解析漂移。未装卷 → nil（转存请求 fail-closed）。
		// 共享卷 owner 前缀隔离由 volume.ResolveLocation 唯一入口承担（见 VolumeFor）；
		// 远程性由目标 FS 自述（syncpkg.LocalVolume）。
		TransferFSFor: func(volumeName string) (syncpkg.FS, string) {
			fsys, scheme, _, ok := h.externalVolumeTransferFS(volumeName)
			if fsys == nil {
				return nil, ""
			}
			if !ok {
				// 卷元信息缺失（极罕见装配异常）：仍加 Guard(Wrap(...))（meta 桶隔离恒生效
				// + 写面恒生成 sidecar，与其余装配形状一致）——由 externalVolumeTransferFS
				// 统一承担。
				return fsys, ""
			}
			return fsys, scheme
		},
		// RecordBypassWrite（R16 缺陷族修复 A/B）：转存/归档产物写目标卷 user 桶不经写内核 ⇒
		// 派生状态（checksum 台账 + 搜索索引）需由写路径显式登记，否则 list 缺条目 / checksum 空。
		RecordBypassWrite: h.recordBypassWrite,
		// VolumePoolChargeFor（R16 P1 修复②/③）：本地卷转存产物的卷级容量池结算/探测
		// ——本地卷 rootfs 无 CapacityFS，卷池权威在路由层 Pool。外部卷不经此（已记）。
		VolumePoolChargeFor: vs.Pool,
		// VolumeFor：转存键空间经 volume.ResolveLocation 计算（权限门/路径安全/共享前缀
		// 由 volume 唯一入口承担，用户裁定 2026-10-05）——装配层提供 vs.ByName 解析。
		VolumeFor: func(volumeName string) (volume.Volume, bool) {
			return vs.ByName(volumeName)
		},
		// TargetQuotaScopeFor：转存产物所在的 owner 配额 Scope（**与删除侧释放、HTTP 上传路由
		// 预留同一解析器** h.quotaScopeFor ⇒ 同一账本对象）。转存写成功后据此结算（遗留②
		// 「幻影释放」修复：不结算则删除该产物的释放会扣同账本其它占用）。
		TargetQuotaScopeFor: h.quotaScopeFor,
		// TargetPhysChargeFor：转存对象的记账字节（**与删除侧 physChargeFor 同一测量函数**
		// filesPhysFootprinter.PhysFootprint）——secretdata 的 pad/parity 逻辑不可推，两处
		// 必须同源，否则写入侧按逻辑记、释放侧按物理放 ⇒ 账本恒偏。
		TargetPhysChargeFor: func(volumeName, owner, remotePath string) (int64, bool) {
			return filesPhysFootprinter{h: h}.PhysFootprint(volumeName, owner, remotePath)
		},
		// IntegrityLookup：完整性语义校验器分发（image/tar 标准库 + ext/video ffprobe，
		// 经 pkg/integrity 注册表）。nil = 语义校验跳过（生产必须装配，否则 --integrity-must-pass
		// 无效果——P1-1 最终审查修复）。
		IntegrityLookup: integrity.Lookup,
	})
	h.storageMgr = sm
}

// buildTunnelHandlers 装配隧道内层 handler：gzip + 速率限制 + CORS 中间件链，
// localMuxGate 角色门禁（任务② MUST-FIX 收口 + RBAC 细分）、writeGuard 写面门、
// requestLog 追踪，并构造 localHandler（xfer listener 直接使用）与 tunnelHandler
// （传统 POST /tunnel 外层帧解密器）。
//
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
func (h *Handlers) buildTunnelHandlers(localMux *http.ServeMux, cfg *Config, log *slog.Logger) {
	// gzip + 速率限制 + CORS 中间件链
	var apiHandler http.Handler = localMux
	apiHandler = GzipMiddleware(log.With("component", "gzip"))(apiHandler)
	if cfg.RateLimit.Enabled {
		rl := NewRateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window, log.With("component", "rate_limiter"))
		// per-IP 桶键升级（设计文档 ip-whitelist §5）：装配了 trusted_proxies 时按真实
		// 客户端 IP 计量（resolveClientIP）；未配置时 SetClientIPFn 不设 → normalizeRemoteIP
		// 等价（零回归）。
		if len(cfg.Auth.TrustedProxies) > 0 {
			rl.SetClientIPFn(func(r *http.Request) string {
				return h.clientIPFromRequest(r)
			})
		}
		// 多实例协调后端：coordinated 开启时按 backend 装配（file = 共享 storage 计数）；
		// 装配失败（未知 backend / file 缺 dir）回退 local + 警告（fail-open 不阻断启动）。
		if cfg.RateLimit.Coordinated {
			if coord, cerr := newCoordinator(cfg.RateLimit.Backend, int64(cfg.RateLimit.Requests), cfg.RateLimit.Window, h.globalRoot.AbsPath(), log); cerr != nil {
				log.Warn("rate limit coordinator setup failed, fallback to local", "error", cerr)
			} else {
				rl.SetCoordinator(coord)
			}
		}
		// per-endpoint 规则 + 全局并发上限装配（roadmap 12.1-6 片 2）：
		// UpdateDimensions 与 PUT /api/config 热更新共用；endpoints 空 + max_concurrent=0
		// = 新维度关闭（零回归，放行链与现状逐字一致）。
		rl.UpdateDimensions(cfg.RateLimit.Endpoints, cfg.RateLimit.EndpointDefault, cfg.RateLimit.MaxConcurrent)
		// 拒绝计数指标装配（/metrics 输出 sproxy_rate_limit_rejected_total）。
		rl.metrics = h.metrics
		h.rateLimiter = rl
		apiHandler = rl.Middleware(apiHandler)
	}
	apiHandler = CORSMiddleware(cfg.CORS, log.With("component", "cors"))(apiHandler)
	// 隧道内层 requireRole 门禁（任务② MUST-FIX 收口）——语义见函数上方大段注释。
	apiHandler = h.localMuxGate(apiHandler)
	// 集群写面门（roadmap 12.1-2 只读副本接入）：writeGuard 非 nil 时写面路由
	// 前置 Authorize()（非主 503）；nil = 单节点零回归。挂 localMuxGate 之后
	// （认证收口后）——门只在身份有效后生效，且对隧道内层/外层统一。
	apiHandler = h.writeGuardMiddleware(apiHandler)
	// 审计中间件（R14 C13）：此前**只**包了主 mux ⇒ 经隧道/CLI 的变更型请求既无传输层兜底审计，
	// 也没有卷维度绑定（多卷部署下 Volume 恒空）。这里与主 mux 用**同一组**中间件（顺序也一致：
	// 卷维度在外、传输兜底在内），避免两条链路语义漂移（新增门禁 R42 钉住同源）。
	apiHandler = h.auditMiddleware(apiHandler)
	apiHandler = h.auditVolumeMiddleware(apiHandler)

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
}

// registerHubRoutes 装配 Hub 中继管理 API（需鉴权）：任意 TCP 流中继、WebRTC 信令桥、
// 节点/统计/服务端点、联邦节点表端点，以及隧道内层用户面裸注册。
// 仅在 opts.RouteTable != nil 时装配（nil = 单机零回归，相关路由 404）。
func registerHubRoutes(h *Handlers, srvMux, localMux *http.ServeMux, opts RegisterRoutesOpts, cfg *Config, log *slog.Logger) {
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
			srvMux.HandleFunc("GET /api/hub/federation/services", h.authMiddleware(h.federationServicesHandler))
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
}

// wireAuditColdArchive 是**冷段归档装配的唯一入口**（评审 R13：集中解析/校验/首次归档）：
//
//  1. 解析归档目的地（`archive_volume` 卷优先，其次 `archive_dir`）；
//  2. `archive_volume` 配置了但解析不到 ⇒ Error（`startup.fail_fast` 时 panic）——必须区分
//     「未配置归档」与「配置了但不可用」，否则运行期归档静默关闭、封段被按份数回收；
//  3. 归档已配置但不可用 ⇒ 标记 Journal：轮转时**整轮跳过**搬移/回收（保留证据）；
//  4. 启动期归档一次（此后由轮转的 OnSeal 回调触发；失败不丢段、下轮重试）。
func wireAuditColdArchive(cfg *Config, set *registry.Set, j *audit.FileJournal, log *slog.Logger) {
	coldFS := newColdArchiveFS(cfg, set, log)
	configured := cfg.Audit.ArchiveDir != "" || cfg.Audit.ArchiveVolume != ""
	warnIfInvalidArchiveVolume(cfg, set, log)
	if cfg.Audit.ArchiveVolume != "" && coldFS == nil {
		msg := "审计归档卷无法解析（未注册/非外部·封装卷）⇒ 冷段归档已关闭；封段仍会被 max_archives 回收"
		if cfg.Audit.Startup.FailFast {
			panic("audit: " + msg + ": " + cfg.Audit.ArchiveVolume)
		}
		log.Error(msg, "archive_volume", cfg.Audit.ArchiveVolume)
	}
	if j != nil {
		j.SetArchiveUnavailable(coldFS == nil && configured)
	}
	runAuditArchive(cfg, coldFS, j, log)
}

// warnIfInvalidArchiveVolume 装配期校验归档卷专用性：不合法则 Error（fail_fast 时 panic）。
// 抽成函数是为了不给 wireAuditColdArchive 增加 gocognit（门禁阈值 15）。
func warnIfInvalidArchiveVolume(cfg *Config, set *registry.Set, log *slog.Logger) {
	name := cfg.Audit.ArchiveVolume
	if name == "" || set == nil {
		return
	}
	v, ok := set.ByName(name)
	if !ok {
		return // 未注册由 newColdArchiveFS 侧处理（另一条告警路径）
	}
	verr := validateAuditArchiveVolume(v, set.Default().Name)
	if verr == nil {
		return
	}
	msg := "审计归档卷配置非法: " + verr.Error()
	if cfg.Audit.Startup.FailFast {
		panic("audit: " + msg)
	}
	log.Error(msg, "archive_volume", name)
}

// validateAuditArchiveVolume 校验 `audit.archive_volume` 的**专用性**（评审 R13 P2）：
//
//  1. **不得是默认卷**：对账（`adjustVolumePool`）按 tenant 桶校准卷占用，而 `audit-archive/`
//     不属于任何 tenant ⇒ 与默认卷同卷时，每轮对账都会把归档字节「修正」掉 ⇒ 卷容量上限被绕过；
//  2. **必须是专用卷**（ACL 白名单模式且白名单为空 ⇒ 任何用户都进不去）：归档的是**证据**，
//     用户可读/可删自己的证据 = 合规语义失效。
func validateAuditArchiveVolume(v volume.Volume, defaultName string) error {
	if v.Name == defaultName {
		return fmt.Errorf("归档卷 %q 不得是默认卷（对账按 tenant 桶校准卷占用，会把归档字节抹掉）", v.Name)
	}
	if v.ACL.Mode != volume.ModeAllow || len(v.ACL.Owners) > 0 {
		return fmt.Errorf("归档卷 %q 必须是专用卷（acl.mode=allow 且白名单为空），当前 mode=%q owners=%d —— 证据不得对普通用户可见/可删",
			v.Name, v.ACL.Mode, len(v.ACL.Owners))
	}
	return nil
}

// runAuditArchive 把封段归档到 `audit.archive_dir`（M4；**复用** pkg/audit 原语）。
//
// 语义（与 pkg/audit.ArchiveSealed 一致）：
//   - 只归档**封段**（活跃段绝不归档）；失败**不丢段**（本地保留、下轮重试）；
//   - 保留最新 `audit.archive_keep` 个封段在本地（0 = 全部归档；建议按查询窗口显式配置）。
//
// 为什么在装配层：归档目的地（挂载的冷卷目录）属部署配置；原语与护栏都在 pkg/audit（单一实现）。
func runAuditArchive(cfg *Config, coldFS syncpkg.FS, j *audit.FileJournal, log *slog.Logger) {
	if coldFS == nil || j == nil {
		return // 未配置归档目的地（或审计未落盘）⇒ 不做任何事
	}
	keep := max(cfg.Audit.ArchiveKeep, 0)
	// 实际写能力来自 pkg/sync（与云端下载→转存同源：FS + WriteIfAbsent 幂等写）；
	// 审计包只做编排与护栏，不自造 sink 接口 ✓。
	arch := audit.FSArchiver{FS: coldFS}
	// 有界超时：OnSeal 回调在**写路径**上同步执行（`Append` 内），归档目标可能是远端/加密卷；
	// 无超时会让某次业务 Emit 被整段上传拖住（评审 R13 P2）。
	ctx, cancel := context.WithTimeout(context.Background(), auditArchiveTimeout)
	defer cancel()
	rep, err := audit.ArchiveSealed(ctx, j, arch, keep)
	if err != nil {
		log.Warn("审计冷段归档失败（封段保留在本地，下轮重试）", "error", err, "archive_dir", cfg.Audit.ArchiveDir)
		return
	}
	if rep.Archived > 0 || rep.Failed > 0 {
		log.Info("审计冷段归档完成", "summary", rep.String(), "archive_dir", cfg.Audit.ArchiveDir)
	}
}

// newSealArchiveHook 返回「封段 → 归档」回调与 journal 绑定函数（M4）。
//
// 护栏：
//   - 回调仅在 `archive_dir` 非空时生效（默认不归档）；
//   - 用 `atomic.Pointer` 拿 journal 自身（回调由轮转触发，早于局部变量赋值时跳过 ✓ 不 panic）；
//   - **TryLock 单飞**：已有归档在跑 ⇒ 本次跳过（下一轮轮转会再次触发，且封段仍在本地 ⇒ 不丢）；
//   - 回调内**不写**同一 Journal（只读段文件 + 归档），因此不会递归轮转 ✓。
func newSealArchiveHook(cfg *Config, coldFS func() syncpkg.FS, log *slog.Logger) (hook func(), bind func(*audit.FileJournal)) {
	var jp atomic.Pointer[audit.FileJournal]
	var mu sync.Mutex
	hook = func() {
		if cfg.Audit.ArchiveDir == "" && cfg.Audit.ArchiveVolume == "" {
			return
		}
		j := jp.Load()
		if j == nil {
			return
		}
		if !mu.TryLock() {
			return
		}
		defer mu.Unlock()
		runAuditArchive(cfg, coldFS(), j, log)
	}
	bind = func(j *audit.FileJournal) { jp.Store(j) }
	return hook, bind
}

// newColdArchiveFS 构造**冷段归档目录**的 FS（`pkg/sync.LocalFS`）。
//
// 收敛点：审计归档（写）与冷段读取（读）**共用同一构造**，且 `sync.NewLocalFS` 在全仓只出现在
// 本文件（R26 门禁白名单只需登记一处；将来换外部/加密卷 FS 也只改这里 ✓）。
func newColdArchiveFS(cfg *Config, set *registry.Set, log *slog.Logger) syncpkg.FS {
	if cfg == nil {
		return nil
	}
	// ① 冷段归档**卷**（优选）：加密卷 ⇒ 冷段自带 at-rest 加密；容量/配额与普通卷一致。
	if name := cfg.Audit.ArchiveVolume; name != "" {
		// 纯解析，**不**在此打日志：本函数在 `/api/audit` 每个请求都会调用一次，
		// 配置错误时逐请求 Warn 会刷爆日志（评审 R13 P1）。装配期统一告警/可 fail-fast。
		if set == nil {
			return nil
		}
		be := set.External(name)
		if be == nil {
			return nil
		}
		return syncpkg.NewPrefixFS(be.FS(), auditArchiveVolumePrefix)
	}
	// ② 裸目录（兼容既有部署）。
	if cfg.Audit.ArchiveDir != "" {
		return syncpkg.NewLocalFS(cfg.Audit.ArchiveDir, log)
	}
	return nil
}

// auditArchiveTimeout 是单次冷段归档的时限（超时后本轮放弃、封段保留、下轮重试）。
const auditArchiveTimeout = 5 * time.Minute

// auditArchiveVolumePrefix 是冷段在**归档卷**内的前缀（与裸目录布局隔离，便于识别与清理）。
const auditArchiveVolumePrefix = audit.DefaultArchivePrefix // 唯一定义在 pkg/audit
