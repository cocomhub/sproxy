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
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// Handlers 持有所有 HTTP handler 的依赖。
type Handlers struct {
	coreDeps
	httpDeps
	authDeps
	filesDeps
	volumeDeps
	cloudDeps
	shareDeps
	hubDeps
	clusterDeps
	auditDeps
	aiDeps
	opsDeps
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

// SetConflictIndex 注入冲突索引（API 数据源；nil 清除，/api/sync/conflicts 返回 400）。
func (h *Handlers) SetConflictIndex(idx *syncmgr.ConflictIndex) {
	h.conflictIndex = idx
}

// LocalHandler 返回隧道内层本地文件 API handler（localMux + 中间件链，
// 不含外层帧解密/密钥检查）。
//
// 供 xfer listener（阶段 5 工作项 1）直接路由解密后的隧道请求：xfer 隧道
// handleStream 已把请求体解密为明文，无需再经 `POST /tunnel` 的外层帧解密
// （NewLocalHandler 期望请求 ctx 带派生密钥且 body 为帧协议——xfer 请求两者皆无，
// 直接使用会 401 unauthorized）。两者互补：`POST /tunnel` 路由用 `h.tunnelHandler`
// 字段做外层帧解密，xfer 隧道用本方法拿明文入站 handler。
// AlertEngine 返回阈值告警引擎（nil = 未启用；装配层只读）。
func (h *Handlers) AlertEngine() *AlertEngine { return h.alertEngine }

// AIInsight 返回 AI 文件洞察器（nil = 未启用；装配层只读）。
func (h *Handlers) AIInsight() *AIInsight { return h.aiInsight }

// SetAIInsight 注入 AI 文件洞察器（装配层调用；nil = 未启用）。
func (h *Handlers) SetAIInsight(a *AIInsight) { h.aiInsight = a }

// SetAIPrivacy 注入 AI 隐私管理器（装配层调用；nil = 未启用）。
func (h *Handlers) SetAIPrivacy(p *AIPrivacy) { h.aiPrivacy = p }

// SetNodeRegistry 注入集群节点注册表（装配层调用；nil = 单节点零回归）。
func (h *Handlers) SetNodeRegistry(r *NodeRegistry) { h.nodeRegistry = r }

// SetShareStore 注入分享存储后端（statestore.md §5.2 装配层改动：nil = 未装配，
// RegisterRoutes 已装配 *ShareStore 本地形态；非 nil = 分享切 StateStore 后端
// （stateBackedShareStore，逐 token key + Consume CAS，见 state_share.go））。
func (h *Handlers) SetShareStore(s ShareStoreIface) { h.shareStore = s }

// Metrics 返回服务的运行时指标（/metrics 渲染 + 装配层统计 hook 注入；nil = 未装配）。
func (h *Handlers) Metrics() *Metrics { return h.metrics }

// SetStateStore 注入状态存储后端（statestore.md §5.2 装配层改动：nil = 未装配，
// checksum/dedup/share/索引保持原本地 JSON 落盘零回归；非 nil = 各 Store 适配器
// 切 StateStore 后端——读旧 meta 回退 + 首写迁新路径的双读单写，见
// state_ledger.go / state_share.go / pkg/files/state_index.go）。
func (h *Handlers) SetStateStore(st state.StateStore) {
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	// 切换后端 ⇒ 关闭并丢弃缓存实例（旧实例的最后写入先落盘，避免跨后端串值）。
	for _, cs := range h.checksumStores {
		if c, ok := cs.(checksum.Closer); ok {
			_ = c.Close()
		}
	}
	clear(h.checksumStores)
	// dedup 台账同样按后端形态缓存，切换后端必须丢弃（否则旧实例仍写旧后端）。
	clear(h.dedupStores)
	h.stateStore = st
}

// EventsBus 返回事件总线（懒建；装配层桥接用）。
func (h *Handlers) EventsBus() *EventBus { return h.eventBus() }

// FileService 返回文件服务域实例（装配层用；nil = 未装配）。
func (h *Handlers) FileService() *files.Service { return h.fileService() }

// PrivacyRoot 返回默认卷根（AI 隐私落盘目标；未装配 → nil）。
func (h *Handlers) PrivacyRoot() *storage.Root {
	tenant := h.tenantFor("")
	if tenant == nil {
		return nil
	}
	return tenant.Root()
}

// SetVectorStore 注入语义搜索向量索引（装配层调用；转发给 fileService）。
func (h *Handlers) SetVectorStore(vs *files.VectorStore) {
	if svc := h.fileService(); svc != nil {
		svc.SetVectorStore(vs)
	}
}

// SetAIEventConsumer 注入 AI 事件消费端（装配层调用；nil = 未启用；Close 时 Stop）。
func (h *Handlers) SetAIEventConsumer(c *AIEventConsumer) { h.aiEventConsumer = c }

// AIEventConsumer 返回装配的 AI 事件消费端（nil = 未启用）。
func (h *Handlers) AIEventConsumer() *AIEventConsumer { return h.aiEventConsumer }

// BackgroundContext 是服务端后台任务的根 ctx（事件消费端审计用；无请求侧 ctx 可用时）。
func BackgroundContext() context.Context { return context.Background() }

// InsightCacheDir 返回 AI 洞察缓存目录（<默认卷 meta>/insight；装配层构造缓存用）。
func (h *Handlers) InsightCacheDir() string {
	tenant := h.tenantFor("")
	if tenant == nil {
		return ""
	}
	return filepath.Join(tenant.Root().AbsPath(), "anonymous", "meta")
}

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
//
// fileServiceIfReady 返回已装配的 fileService（懒建**已触发**时才可用；测试直构造 h 未装配
// tenants 时返回 nil——调用方回落内部实现，不 panic）。用 filesOnce 的原子 done 标志避免
// 读 h.filesSvc 与 fileService 懒建写之间的 race。
func (h *Handlers) fileServiceIfReady() *files.Service {
	h.filesMu.Lock()
	defer h.filesMu.Unlock()
	if h.filesSvc != nil {
		return h.filesSvc
	}
	if h.tenants == nil {
		return nil
	}
	return h.fileServiceLocked()
}

func (h *Handlers) fileService() *files.Service {
	h.filesMu.Lock()
	defer h.filesMu.Unlock()
	return h.fileServiceLocked()
}

// fileServiceLocked 调用方持 h.filesMu；懒装配（filesOnce 幂等）。
func (h *Handlers) fileServiceLocked() *files.Service {
	h.filesOnce.Do(func() {
		rt := filesRuntime{h: h}
		opts := []files.Option{
			files.WithLogger(func() *slog.Logger { return h.logger }),
			files.WithActor(rt),
			files.WithVolumes(rt),
			files.WithQuota(rt),
			files.WithQuotaWait(func() time.Duration { return h.quotaWaitTimeout }),
			files.WithChecksumLedger(rt),
			files.WithDownloadPaths(rt),
			files.WithFileLocks(rt),
			// F1：外部卷对象的硬删接缝（外部卷无 *storage.Root，其对象此前无用户可见删除入口）。
			files.WithExternalOps(externalVolumeOps{h: h}),
			// 卷类型分类（决策 3：去领域侧硬编码 wrapper 名单）——分类事实源在
			// registry.CategoryOf，经 filesVolumeSet 转发注入。
			files.WithVolumeTypeClassifier(filesVolumeSet{Set: h.volSet, sinkHost: h}),
			files.WithChunkedUploads(rt),
			files.WithVersioning(rt),
			files.WithDedup(rt),
			files.WithAudit(rt),
			files.WithEventSink(rt),
			files.WithUploadBodyLimit(func() int64 { return int64(h.cfgPtr.Load().MaxUploadBytes) }),
			files.WithBandwidthLimiter(rt),
		}
		// meta 恒生成（用户裁定：只关校验不关 meta）——fileMetaPolicy 恒装配。
		// skipFileMetaAssembly 是测试隔离开关（AGENTS.md 7b：测试注入走结构体内部私有
		// 变量；存量语义测试用 newAssemblyTestHandlers 置 true 跳过 meta 装配，避免
		// 配额/统计/账本断言被 sidecar 字节干扰；生产恒 false 装配 meta 策略）。
		if !h.skipFileMetaAssembly {
			opts = append(opts, files.WithFileMeta(filesMetaPolicy{h: h}))
		}
		if h.metrics != nil {
			opts = append(opts, files.WithMetrics(h.metrics))
		}
		// 卷自报物理足迹（配额模型 v3：只按物理占用记账；secretdata 的 parity/meta pad 无法由
		// 逻辑长度推出 ⇒ 必须由卷实测）。未装配外部卷 ⇒ 无该能力 ⇒ 写内核回落既有折算。
		opts = append(opts, files.WithPhysFootprinter(filesPhysFootprinter{h: h}))
		// 唯一必需项（TenantResolver）由 *storage.TenantCache 直接满足——它绑定默认卷根，
		// 与 pkg/server 的 tenantFor 同一实例（单一租户缓存），无需适配类型。
		svc, err := files.New(h.tenants, opts...)
		if err != nil {
			// 唯一必需项（TenantResolver）由 rt 提供，不可达；保留 fail-fast 以防未来改动。
			panic("files.New 装配失败: " + err.Error())
		}
		h.filesSvc = svc
		// V3：本地卷结构性变异 → 搜索索引自动维护（唯一接线点，见
		// volume_mutation_wiring.go）。放在 Once 内直接用局部 svc，避免递归调用
		// fileService()（sync.Once 自锁）。
		h.wireLocalVolumeMutations(svc)
	})
	return h.filesSvc
}

// filesMu 保护 filesSvc 懒装配（fileService 写 vs fileServiceIfReady 读）。
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
// checksumRel 是校验和台账在**租户卷内**的相对路径（<租户根>/meta/checksums.json）。
// 台账经租户**卷句柄**读写：卷自己决定是否加密（使用方不理解加密态），并天然继承封印判定。
const checksumRel = "meta/checksums.json"

// stateStore 非 nil（集群模式装配）时返回 StateStore 适配器（读旧 meta 回退 + 首写迁
// StateStore 新路径，双读单写零回归，见 state_ledger.go）。
func (h *Handlers) checksumStoreFor(owner string) checksum.ChecksumStoreIface {
	owner = normalizeOwner(owner)
	// 先取租户（内部锁 tenantMu，懒创建租户根 + meta 目录）。
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return nil
	}
	root := tnt.Root()
	if root == nil {
		h.logger.Warn("租户卷句柄不可用", "owner", owner)
		return nil
	}
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if cs, ok := h.checksumStores[owner]; ok {
		return cs
	}
	var cs checksum.ChecksumStoreIface
	if h.stateStore != nil {
		// **必须缓存**：每次新建 = 每次首读都从 StateStore 全量拉回 + 反序列化（O(全库) KV 读），
		// 且写合并器随实例丢弃而失效。
		cs = checksum.NewStateBackedChecksumStore(h.stateStore, "checksum/"+owner+"/all", root, checksumRel, h.logger)
	} else {
		cs = checksum.NewChecksumStore(root, checksumRel, h.logger)
	}
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
		us.SetStorageMgr(filesStorageManager{m: h.storageMgr, waitTimeout: h.quotaWaitTimeout})
	}
	h.uploadStores[owner] = us
	return us
}

// dedupStoreFor 返回 owner 的 per-tenant 去重台账（懒创建，缓存到 map，与 checksumStoreFor 同构）。
// 台账路径 = <tenant meta>/dedup.json；dedup 未启用或获取不到租户返回 nil。
// 缓存复用同一实例：台账内存态增量保存回磁盘，避免高频上传每次磁盘 Load（#424 残余优化）。
// stateStore 非 nil（集群模式装配）时返回 StateStore 后端（读旧 meta 回退 + 首写迁新路径，
// 双读单写零回归，见 state_ledger.go）。
func (h *Handlers) dedupStoreFor(owner string) *files.DedupStore {
	if !h.cfgPtr.Load().Dedup.Enabled {
		return nil
	}
	owner = normalizeOwner(owner)
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return nil
	}
	metaAbs, ok := tnt.Root().Abs("meta")
	if !ok {
		h.logger.Warn("派生租户 meta 路径失败", "owner", owner)
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
	var ds *files.DedupStore
	if h.stateStore != nil {
		// **必须缓存**（与 checksumStoreFor 同构）：每次新建 = 每次首读从 StateStore 全量拉回 +
		// 反序列化（O(全库) KV 读），且后续写入落在随后被丢弃的实例上。
		ds = files.NewDedupStore(filepath.Join(metaAbs, fileNameDedup), h.logger, &files.DedupStateOptions{
			St:         h.stateStore,
			Key:        "dedup/" + owner + "/all",
			LegacyPath: filepath.Join(metaAbs, fileNameDedup),
		})
	} else {
		ds = files.NewDedupStore(filepath.Join(metaAbs, fileNameDedup), h.logger)
	}
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
var quotaBucketNames = storage.QuotaBucketNames()

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
		bucketLimits = quotaBucketLimitsFromCfg(cfg)
	}
	s := h.globalPool.Scope("/tenant/"+owner, quotaBytes)
	if s == nil {
		// **超售 ⇒ fail-closed**（独立复审 B-P2-4 根因修复 + 用户裁定启用 I1 强制）：Σ 租户配额
		// 超过全局上限 ⇒ 分配被 I1 原子拒绝。**不能**返回 nil（那会让写路径**静默无配额**）⇒
		// 返回**拒写配额树**（任何非零预留失败 ⇒ 写被 507 拒绝）。**不缓存**：管理员调大全局上限
		// 后下一个请求自动恢复。
		h.logger.Error("tenant quota allocation refused by I1 (Σ owner quotas exceed global limit); "+
			"writes for this owner will be rejected until the global limit is raised",
			"owner", owner, "owner_quota_bytes", quotaBytes)
		return h.deniedTenantQuota()
	}
	buckets := make(map[string]*quota.Scope, len(quotaBucketNames)+len(bucketLimits))
	for _, b := range quotaBucketNames {
		buckets[b] = s.Mount(b, 0)
	}
	mountBucketLimitScopes(buckets, bucketLimits)
	h.quotaScopes[owner] = s
	h.quotaBuckets[owner] = buckets
	return s, buckets
}

// deniedTenantQuota 返回**拒写**的租户配额树：全局上限 1 字节 ⇒ 任何 >1 字节的预留必失败
// （超售导致租户配额无法分配时的 fail-closed 载体；见 ensureTenantQuotaLocked）。
// 所有节点上限传 0（不限制）⇒ 建树本身永不因 I1 失败（可安全作为兜底）。
func (h *Handlers) deniedTenantQuota() (*quota.Scope, map[string]*quota.Scope) {
	pool := quota.NewPool(1)
	s := pool.Scope("/tenant/_denied", 0)
	buckets := make(map[string]*quota.Scope, len(quotaBucketNames))
	for _, b := range quotaBucketNames {
		buckets[b] = s.Mount(b, 0)
	}
	return s, buckets
}

// quotaBucketLimitsFromCfg 复制 bucket_limits 配置到 int64 map（装配期硬配置，懒建后
// 缓存不重建 → SIGHUP 后修改不生效，重启进程）。
func quotaBucketLimitsFromCfg(cfg *Config) map[string]int64 {
	if cfg == nil || cfg.BucketLimits == nil {
		return nil
	}
	bucketLimits := make(map[string]int64, len(cfg.BucketLimits))
	for k, v := range cfg.BucketLimits {
		bucketLimits[k] = int64(v)
	}
	return bucketLimits
}

// mountBucketLimitScopes 按 bucket_limits 配置建精确路径子 Scope（http route 式嵌套）。
// 子目录 Scope 沿父链聚合到功能桶 → 租户 → 全局，对子 Scope 记一笔账即自动逐级检查
// 所有层级上限；quotaBuckets map 保留配置键 → Scope 引用（供配置校验/测试/旧调用）。
func mountBucketLimitScopes(buckets map[string]*quota.Scope, bucketLimits map[string]int64) {
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

// ownerQuotaScope 返回 owner 的**根配额 Scope**（该 owner 全部用量的聚合点）。
//
// **为何需要它（而不是 `quotaScopeFor(owner, "")`）**：`quotaScopeFor` 首行
// `segs := strings.Split(rel, "/")` ⇒ `rel == ""` 时 `segs[0] == ""` ⇒ **恒返回 nil** ✗
// ⇒ 调用方会静默拿到「无配额」✗（`rel == ""` 的调用点全仓仅一处：配额预警读取器 ✓）。
//
// **为何根 Scope 的 Usage 就是「该 owner 的用量」**（2026-10-16 核实 ✓）：
// `type Scope struct{ pool *Pool }`，而 `Mount`/`EnsureScope` 经 `newScope` 创建**子 Pool**
// （`parent: p`，各自持有 `committed`/`maxBytes` ✓）⇒ `Scope.Usage()` 返回 **`p.committed`
// = 该节点自己的**用量 ✓（**不是**全局池用量 ✗）；且 `commitUp` 沿 `parent` 链逐层累计 ⇒
// **根节点 committed = 该 owner 全子树用量** ✓、`MaxBytes()` = 该 owner 的配额上限 ✓。
func (h *Handlers) ownerQuotaScope(owner string) *quota.Scope {
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	root, _ := h.ensureTenantQuotaLocked(owner)
	return root
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

// stagingQuotaScope 返回 owner 的**独立 staging 配额 Scope**（用户裁定 2026-10-10）：
// 本地上传暂存**独立记账**（不与网盘 owner_quotas 混用）——外部卷 WriteFile 先预留本地
// 字节、写后释放，防本地磁盘被 staging 占满。上限 = trusted_volume.staging_quota_bytes
// （0/缺省 = 不限制但记账，预留/释放对称不误伤）。
//
// **M5 修复（2026-10-11，第 7 轮对抗评审）**：本 Scope 现挂**独立根池** `h.stagingPool`
// （容量 = `staging_quota_bytes`），不再挂 `globalPool` 之下。原因：挂父链会让慢速外部卷
// 上传（长期占本地暂存）抬高 `max_storage_bytes` 的全局 usage，从而**误拒无关 owner 的
// 正常写**（跨租户干扰）；「staging 占物理盘」的保护改由运维显式配置 `staging_quota_bytes`
// 承担（建议按本地盘余量设置）。globalPool 未装配时仍返回 nil（无配额能力零回归）。
// 懒建缓存（tenantMu 保护），装配期硬配置不重建。
func (h *Handlers) stagingQuotaScope(owner string) *quota.Scope {
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if h.globalPool == nil {
		return nil
	}
	if h.stagingScopes == nil {
		h.stagingScopes = make(map[string]*quota.Scope)
	}
	if sc, ok := h.stagingScopes[owner]; ok {
		return sc
	}
	if h.stagingPool == nil {
		// 独立根池：容量 = staging_quota_bytes（0 = 不设上限，仅记账；根池无父链，
		// 不牵连 max_storage_bytes 的全局账本——M5 修复）。
		var poolMax int64
		if cfg := h.cfgPtr.Load(); cfg != nil {
			poolMax = int64(cfg.TrustedVolume.StagingQuotaBytes)
		}
		h.stagingPool = quota.NewPool(poolMax)
	}
	var limit int64
	if cfg := h.cfgPtr.Load(); cfg != nil {
		limit = int64(cfg.TrustedVolume.StagingQuotaBytes)
	}
	sc := h.stagingPool.Scope("/staging/"+owner, limit)
	h.stagingScopes[owner] = sc
	return sc
}

// stagingQuotaTrackerFor 返回 owner 的 staging 配额钩子（独立 Scope 适配——
// ReserveUsage = TryReserve + Commit；不足排队等待（quota.StagingTracker：sync.Cond +
// ctx/超时）；ReleaseUsage = ReleaseUsage + 广播。无 globalPool → nil（零回归）。
// StagingQuotaTrackerFor 导出版（供装配层把同一 per-owner staging tracker 注入
// 非 HTTP 写入口，如 baidupcs sync push——P1：此前误用 owner user 桶 Scope）。
func (h *Handlers) StagingQuotaTrackerFor(owner string) syncpkg.StagingQuotaTracker {
	return h.stagingQuotaTrackerFor(owner)
}

func (h *Handlers) stagingQuotaTrackerFor(owner string) syncpkg.StagingQuotaTracker {
	sc := h.stagingQuotaScope(owner)
	if sc == nil {
		return nil
	}
	// 按 owner 复用同一 tracker（含 cond）——跨请求 ReleaseUsage 广播才能唤醒等待者。
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if h.stagingTrackers == nil {
		h.stagingTrackers = make(map[string]*quota.StagingTracker)
	}
	if t, ok := h.stagingTrackers[owner]; ok {
		return t
	}
	t := quota.NewStagingTracker(sc)
	h.stagingTrackers[owner] = t
	return t
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
