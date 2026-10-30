// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package syncexec 提供 pkg/syncmgr.Executor 的基于 pkg/sync 引擎的实现。
//
// 模块边界：pkg/syncmgr 不依赖 pkg/sync（其 HTTPTransport 依赖 pkg/client，会与 pkg/client
// 的 e2e 测试形成 import cycle）。因此实际同步执行放在本包，由 cmd/sproxy 装配注入
// syncmgr.Manager。
//
// 依赖方向：本包是 pkg/syncmgr 之上的**消费者**（syncmgr 管任务，本包管执行）。2026-09
// 之前 syncmgr 住在 pkg/server 里，本包因此反向依赖了装配层——那是全仓唯一一条生产分层
// 倒置，已由 S1 抽取修正，并由门禁 R4（领域包不得导入装配层）永久把守。
package syncexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/sync/httptransport"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// Executor 基于 pkg/sync.Engine 的同步执行器（实现 syncmgr.Executor）。
type Executor struct {
	// TenantRoot 按任务 owner 解析租户 user 根绝对路径（push 的 src / pull 的 dst 根）。
	// 装配层注入（如 Handlers.syncTenantRoot）；nil 时 Run 报错（fail-closed）。
	TenantRoot syncmgr.TenantRootResolver
	// TenantRootFS 是**可选**的「Root 感知本地 FS 提供者」：按 owner 返回根在该租户**租户根**
	// （user 桶父目录）的 FS。装配层注入 `server.NewRootFS(tnt.Root())` ⇒ 本地侧读写经
	// `storage.Root` 入口，自动获得 **at-rest 透明加解密**与**封印**语义（评审 P0：此前本地侧
	// 一律裸 `NewLocalFS(绝对路径)`——加密卷上 pull 落明文、push 上传密文）。
	// nil = 回落路径式 LocalFS（测试/未接线装配；生产应注入）。
	TenantRootFS func(owner string) (syncpkg.FS, bool)
	// TenantPhysSize 是**可选**的「记账口径提供者」：按 owner 返回本卷的**物理**字节折算
	// （nil = 恒等）。与 `TenantRootFS` **必须同源**：`TenantRootFS` 返回的 FS 经
	// `NewRootFS` **透明加密**落盘（物理 > 逻辑），而逐文件配额记账用的是源条目 `size`
	// （**逻辑**）⇒ 不折算会**少记** `(物理 − 逻辑)`（owner 配额侧永不收敛 ⇒ 可超真实配额，G1）。
	// 装配层注入 `h.SyncTenantPhysSize`（`files.RootPhysSizer` 同源）。
	TenantPhysSize func(owner string) func(int64) int64
	// TenantScope 按任务 owner 解析租户 user 桶配额 Scope（nil 时不启用逐文件预留，
	// 由 syncmgr 占位/对账机制兜底——兼容未装配 Scope 的旧装配）。写前逐文件
	// TryReserve(文件 size) 是"写前 guard"，写成功 Commit(actual) 使 user 桶等额入账。
	TenantScope func(owner string) *quota.Scope
	// ScopeFor 按 (owner, rel) 解析配额子 Scope（rel 含功能桶前缀，如 "user/dir/f.txt"；
	// 最长前缀命中 bucket_limits 子目录）。优先于 TenantScope：逐文件预留时按文件实际
	// 路径路由，子目录配额对 sync pull 生效；未装配时退化为 nil（直写，由占位对账兜底）。
	ScopeFor func(owner, rel string) *quota.Scope
	// Logger 是执行日志。
	Logger *slog.Logger
	// ConflictIndex 是冲突索引存储（merge3 冲突登记回调落库；nil = 不登记——
	// 仅写带标记文件，零回归）。装配层创建后注入。
	ConflictIndex *syncmgr.ConflictIndex
	// MeshFS 是**装配层注入**的 mesh 版 sync.FS 工厂（Y 二期 P3-d）。
	//
	// 为什么是工厂而不是 FS 实例：mesh 载体需要按远端配置（node/volume/pins/transport）拨号并
	// 建链，且 `pkg/tunnel/mesh` 是**独立 module**——`pkg/syncexec` 既不得依赖它、也不该自己
	// 拨号。装配层（cmd/sproxy）用 `pkg/remote` + 具体拨号器实现本函数注入。
	//
	// nil = 未装配 mesh 载体：`kind=mesh` 的远端明确报 ErrMeshTransportNotWired（**绝不回落
	// direct**——回落会让「已声明 mesh 授权」的配置静默走直连凭据，破坏授权语义）。
	MeshFS MeshFSFactory
	// BaidupcsFS 是**装配层注入**的本机百度网盘卷 FS 工厂（P4）。
	//
	// 与 mesh 同构：`pkg/syncexec` 不得依赖 `pkg/volume/ext/baidupcs` 具体类型、也不自己装配网盘卷
	// （卷名/凭据/中间态目录全在装配层）。工厂按远端配置的 Volume（本机卷名）构造对应
	// StorageFS 并返回 close。
	//
	// nil = 未装配 baidupcs 载体：`kind=baidupcs` 的远端明确报 ErrBaidupcsNotWired（**绝不
	// 回落 direct**——回落会让「已声明本机卷」的配置静默走远程 HTTP，破坏卷寻址语义）。
	BaidupcsFS BaidupcsFSFactory
}

// MeshFSFactory 按远端配置构造 mesh 版 `sync.FS`，并返回任务结束时调用的 close（关链路）。
type MeshFSFactory func(ctx context.Context, remote syncmgr.RemoteConfig) (syncpkg.FS, func(), error)

// BaidupcsFSFactory 按远端配置构造本机卷版 `sync.FS`，并返回任务结束时调用的 close（关链路）。
// 调用方只消费 sync.FS 抽象；owner 为任务归属（P5：staging 配额按 owner 分桶需要——
// 工厂可据 owner 解析对应配额 Scope 装配到 StorageFS）。
type BaidupcsFSFactory func(ctx context.Context, remote syncmgr.RemoteConfig, owner string) (syncpkg.FS, func(), error)

// CarrierReporter 是远端 FS 的**可选**扩展点：报告本次运行期间实际使用过的载体计数。
//
// 键为载体名（`webrtc` / `relay`）；实现方语义：**每次成功建立链路计一次**，可同时出现多个键
// （`auto` 下混合使用）。`sync.FS` 接口本身不含载体概念（本地 FS、HTTP 直连都没有），故用可选
// 接口而不是改 `sync.FS` 形状——不实现即留空，零回归。
type CarrierReporter interface {
	CarrierStats() map[string]int
}

// SetMeshFSFactory 注入 mesh 载体工厂（装配层在 newExecutor 后调用；未注入时 mesh 远端
// fail-closed）。
func (e *Executor) SetMeshFSFactory(f MeshFSFactory) { e.MeshFS = f }

// SetBaidupcsFSFactory 注入 baidupcs 载体工厂（装配层在 newExecutor 后调用；未注入时
// kind=baidupcs 远端 fail-closed）。
func (e *Executor) SetBaidupcsFSFactory(f BaidupcsFSFactory) { e.BaidupcsFS = f }

// SetTenantScopeResolver 注入 user 桶配额 Scope 解析器（装配层在 newExecutor 后调用；
// 测试用独立 quota.Pool 建 Scope）。未注入时逐文件预留关闭。
func (e *Executor) SetTenantScopeResolver(f func(owner string) *quota.Scope) { e.TenantScope = f }

// SetScopeResolver 注入按 (owner, rel) 解析配额子 Scope 的解析器（bucket_limits 子目录
// 路由；未注入时回落 TenantScope）。
func (e *Executor) SetScopeResolver(f func(owner, rel string) *quota.Scope) { e.ScopeFor = f }

// NewExecutor 创建执行器。tenantRoot 由装配层注入（见 syncmgr.TenantRootResolver）。
func NewExecutor(tenantRoot syncmgr.TenantRootResolver, logger *slog.Logger) *Executor {
	return &Executor{TenantRoot: tenantRoot, Logger: logger}
}

func (e *Executor) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// conflictRecorder 返回 merge3 冲突登记回调（Engine.ConflictRecorder 注入用）。
// nil ConflictIndex = 不登记（仅写带标记文件，零回归）；装配层注入后冲突自动落库。
func (e *Executor) conflictRecorder() func(syncpkg.ConflictRecord) {
	if e.ConflictIndex == nil {
		return nil
	}
	return func(rec syncpkg.ConflictRecord) {
		e.ConflictIndex.Record(rec)
		if e.Logger != nil {
			e.Logger.Info("sync conflict recorded", "path", rec.Path, "hunks", rec.HunkCount)
		}
	}
}

// userRootFor 返回任务 owner 租户 user 根绝对路径（LocalFS 根）。解析失败返回错误
// （fail-closed：租户不可用时任务失败，绝不回落全局根）。
func (e *Executor) userRootFor(owner string) (string, error) {
	if e.TenantRoot == nil {
		return "", fmt.Errorf("同步执行器未配置租户根解析器")
	}
	userRoot, _, ok := e.TenantRoot(owner)
	if !ok {
		return "", fmt.Errorf("租户不可用: %q", owner)
	}
	return userRoot, nil
}

// quotaLocalFS 是 pull 本地写侧的配额感知包装（FS 装饰器）：每次 WriteFile 前按文件
// size 在按文件实际 rel 解析的 user 桶子 Scope 上 TryReserve（写前 guard，最长前缀命中
// bucket_limits 子目录时受该子目录上限约束，父链聚合逐级检查），写成功 Commit(actual)
// 使配额等额入账、失败 Release 归还。覆盖写（overwrite）场景：engine 先 Rename 目标到
// .sync-tmp 再写新文件——新文件字节先 TryReserve 落账；旧文件字节仍占 user 桶（本装饰
// 器不事后释放旧字节），由 syncmgr reconcile（占位释放）与周期扫描 reconcile（Adjust
// 到磁盘）校准覆盖写后的净占用。TryReserve 失败返回 ErrStorageFull（该文件 ActionError，
// 不中止整体同步）；scopeFor 为 nil（未装配）时退化为直写（旧行为）。
type quotaLocalFS struct {
	inner syncpkg.FS
	// owner 为任务 owner；scopeFor 按 (owner, rel) 解析配额子 Scope（nil 时不启用逐文件
	// 预留）。只留解析器在 WriteFile 时按 relPath 解析——bucket_limits 子目录配额对
	// sync pull 同样生效。
	owner    string
	scopeFor func(owner, rel string) *quota.Scope
	// phys 是本卷的**记账口径折算**（nil = 恒等；加密卷 ⇒ `EncryptedPhysicalSize`）。
	// 见 `Executor.TenantPhysSize` 的注释：本地目标经 `NewRootFS` 透明加密落盘，
	// 而 `size` 是源条目的**逻辑**长度 ⇒ 不折算会少记（可超真实配额，G1）。
	phys func(int64) int64
}

// physSize 把**逻辑**字节折算为本卷**记账口径**（物理）；nil / 非正值 ⇒ 原样（明文卷零回归）。
func (q *quotaLocalFS) physSize(n int64) int64 {
	if n <= 0 || q.phys == nil {
		return n
	}
	return q.phys(n)
}

func (q *quotaLocalFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return q.inner.ListDir(ctx, p)
}
func (q *quotaLocalFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return q.inner.Stat(ctx, p)
}
func (q *quotaLocalFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return q.inner.OpenRead(ctx, p)
}
func (q *quotaLocalFS) Rename(ctx context.Context, f, t string) error {
	return q.inner.Rename(ctx, f, t)
}
func (q *quotaLocalFS) Delete(ctx context.Context, p string) error  { return q.inner.Delete(ctx, p) }
func (q *quotaLocalFS) MakeDir(ctx context.Context, p string) error { return q.inner.MakeDir(ctx, p) }

// WriteFile 先按 relPath 解析子 Scope 并 TryReserve(size)，写成功 Commit(actual) 入账；
// 失败 Release 归还。scopeFor 为 nil 时退化为直写。
func (q *quotaLocalFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, relPath string, r io.Reader, size, mtime int64) error {
	scope := q.examScope(relPath)
	if scope == nil {
		return q.inner.WriteFile(ctx, key, relPath, r, size, mtime)
	}
	// 记账口径（2026-10-16 高风险区审核修复）：本地目标经 `TenantRootFS`（生产 = `NewRootFS`）
	// **透明加密落盘** ⇒ 预留与结算都必须按**物理**字节；`size` 是源条目的**逻辑**长度。
	charge := q.physSize(size)
	// **刻意不排队等待**（用 TryReserve 而非 WaitReserve）：逐文件 guard 的语义是
	// 「该文件因配额失败（ActionError）并**继续同步其余文件**」，不是「阻塞 30s 等空间」——
	// 改成等待会让一个配额不足的文件把整轮同步拖住（实测 `TestQuota_TwoConcurrentPulls_*`
	// 由「立即失败」变为 30s 超时 ✗）。整任务的排队重试由 syncmgr 的占位/对账承担。
	res, err := scope.TryReserve(charge)
	if err != nil {
		return err // 配额不足：该文件失败（ActionError），不中止整体同步
	}
	// 委托 inner.WriteFile（其 copyWithCtx 已做 ctx 感知拷贝）。成功 Commit(charge)——
	// 源条目 size 即已知真实**逻辑**大小（与 engine 的 BytesDone/进度一致），入账前折算为物理；
	// 失败 Release 归还。
	if werr := q.inner.WriteFile(ctx, key, relPath, r, size, mtime); werr != nil {
		res.Release()
		return werr
	}
	res.Commit(charge)
	return nil
}

// examScope 按 relPath 解析配额子 Scope（relPath 相对 user 桶根；补 "user/" 前缀后交给
// scopeFor 按 owner 路由——bucket_limits 子目录配额对 sync pull 生效）。scopeFor 为 nil
// 时返回 nil（退化为直写）。
func (q *quotaLocalFS) examScope(relPath string) *quota.Scope {
	if q.scopeFor == nil {
		return nil
	}
	return q.scopeFor(q.owner, "user/"+relPath)
}

var _ syncpkg.FS = (*quotaLocalFS)(nil)

// scopeFor 是 Executor 的 resolver：优先按 (owner, rel) 路由（bucket_limits 子目录），
// 未注册时回落 e.TenantScope（仅按 owner 取 user 桶）。
func (e *Executor) scopeFor(owner, rel string) *quota.Scope {
	if e.ScopeFor != nil {
		return e.ScopeFor(owner, rel)
	}
	return e.tenantScope(owner)
}

// tenantScope 返回 owner 的 user 桶配额 Scope（按注入的解析器；nil 时返回 nil）。
func (e *Executor) tenantScope(owner string) *quota.Scope {
	if e.TenantScope == nil {
		return nil
	}
	return e.TenantScope(owner)
}

// Run 执行一次同步（实现 syncmgr.Executor）。
//
// 远程访问方式（HTTP 直连）：Dial = net.Dial 到 remote.URL 的 host:port，走远程 sproxy
// 的 HTTP 文件 API（SproxySig 认证）。mesh 通道为后续增强——Dial 由配置驱动，未来可注入
// mesh 拨号器（HTTPTransportConfig.Dial 是注入点）。
func (e *Executor) Run(ctx context.Context, task *syncmgr.SyncTask, remote syncmgr.RemoteConfig) (*syncmgr.RunResult, error) {
	// 本地端（push 的 src / pull 的 dst）按任务 owner 解析到租户 user 根（<root>/<tenant>/user）。
	// 布局迁移后由注入的 TenantRoot 解析器派生（与 pkg/server 租户布局单一来源，owner
	// 校验集中到 pkg/storage，消除双端漂移）；owner 由服务端派生（可信），解析失败 fail-closed。
	localRoot, err := e.userRootFor(task.Owner)
	if err != nil {
		return nil, err
	}
	if mkErr := os.MkdirAll(localRoot, 0o755); mkErr != nil {
		return nil, fmt.Errorf("创建本地同步根 %s 失败: %w", localRoot, mkErr)
	}
	srcFS, dstFS, remoteClosers, err := e.prepareDirectionFS(ctx, task, remote, localRoot, filepath.Dir(localRoot))
	if err != nil {
		return nil, err
	}
	defer closeRemoteFS(remoteClosers)

	// 双向（both）：push+pull 合并为一次任务——先 push（本地→远程）再 pull（远程→本地）。
	// 语义（roadmap P0）：一次提交两边一致；两端各自新增/修改在任务内互相传播；
	// 删除传播与冲突策略由任务参数控制。
	if task.Direction == string(syncmgr.DirectionBoth) {
		return e.runBoth(ctx, task, remote, localRoot, filepath.Dir(localRoot))
	}

	job := buildJob(task)

	engine := &syncpkg.Engine{Logger: e.logger(), ConflictRecorder: e.conflictRecorder()}
	syncErr := engine.Sync(ctx, srcFS, dstFS, job)
	verifyFailed := e.verifyAfterSync(ctx, srcFS, dstFS, job)

	result := e.buildRunResult(job, verifyFailed, srcFS, dstFS)
	e.applyFailureStatus(result, job, syncErr)
	return result, syncErr
}

// closeRemoteFS 依次关闭所有远端 FS 的 close 回调（幂等：nil 跳过）。
func closeRemoteFS(remoteClosers []func()) {
	for _, closeFn := range remoteClosers {
		if closeFn != nil {
			closeFn()
		}
	}
}

// prepareDirectionFS 按方向准备 src/dst FS：push（本地→远程）；pull（远程→本地）。
// 返回待延迟关闭的远端 close 回调列表。
func (e *Executor) prepareDirectionFS(ctx context.Context, task *syncmgr.SyncTask, remote syncmgr.RemoteConfig, localRoot, tenantRoot string) (syncpkg.FS, syncpkg.FS, []func(), error) {
	if task.Direction == string(syncmgr.DirectionPush) {
		// **键空间**：push 源的引擎键空间是 **user 桶相对**（job.Src 为空 = 桶根），而
		// `localFS` 返回的是**租户根** FS（Root 感知提供者注入后）⇒ 必须补 `user/` 前缀，
		// 否则会把整个租户根（含 meta/ 等内部桶）当源推送（评审第 2 轮 P0 回归）。
		srcFS := e.pushSourceFS(tenantRoot, task.Owner)
		remoteFS, closeRemote, err := e.newRemoteFS(ctx, remote, task.Owner)
		if err != nil {
			return nil, nil, nil, err
		}
		return srcFS, remoteFS, []func(){closeRemote}, nil
	}
	remoteFS, closeRemote, err := e.newRemoteFS(ctx, remote, task.Owner)
	if err != nil {
		return nil, nil, nil, err
	}
	dstFS := &quotaLocalFS{
		inner: e.newLocalPullFS(tenantRoot, task.Owner),
		owner: task.Owner,
		// 解析器按 (owner, rel) 路由 bucket_limits 子目录配额；e.tenantScope 仅按 owner
		// 取 user 桶——由 syncexec 统一补 "user/" 前缀交给注册的 scopeFor（若为 nil 则
		// 该租户无配额，退化为直写）。装配层注入的 ParseScope 见 Handlers.SyncQuotaScope。
		scopeFor: e.scopeFor,
		phys:     e.physFor(task.Owner),
	}
	return remoteFS, dstFS, []func(){closeRemote}, nil
}

// pushSourceFS 返回 push 方向的**源 FS**：引擎键空间是 **user 桶相对**（`job.Src` 空 = 桶根），
// 而 `localFS` 返回**租户根** FS ⇒ 必须叠加 `user/` 前缀（否则会把整个租户根——含 meta/ 等内部
// 桶——当源推送；评审第 2 轮 P0 回归）。抽为单一事实源，供 push 与 runBoth 共用。
func (e *Executor) pushSourceFS(tenantRoot, owner string) syncpkg.FS {
	return syncpkg.NewPrefixFS(e.localFS(tenantRoot, owner), "user/")
}

// physFor 返回该 owner 本卷的**记账口径折算**（nil = 恒等）。与 `TenantRootFS` 同源注入：
// 本地目标经 `NewRootFS` 透明加密落盘 ⇒ 逐文件配额必须按**物理**字节记（见字段注释）。
func (e *Executor) physFor(owner string) func(int64) int64 {
	if e.TenantPhysSize == nil {
		return nil
	}
	return e.TenantPhysSize(owner)
}

// localFS 返回本地侧 FS：优先 **Root 感知**提供者（加密/封印语义正确），否则回落路径式 LocalFS。
// 生产装配注入 TenantRootFS；测试可注入替身或留 nil（零回归）。
func (e *Executor) localFS(localRoot, owner string) syncpkg.FS {
	if e.TenantRootFS != nil {
		if fs, ok := e.TenantRootFS(owner); ok && fs != nil {
			return fs
		}
	}
	return syncpkg.NewLocalFS(localRoot, e.logger())
}

// newLocalPullFS 构造 pull 方向的本地目标 FS：租户根上的「user 桶视图」× trusted.Wrap。
//
// 安全/完整性背景（第 5 轮对抗评审 P1）：`pkg/syncexec` 全包零 meta 引用——pull/both
// 的本地落盘不重建也不失效 sidecar，导致（a）覆盖已上传文件后旧 sidecar 描述旧内容 →
// `/download` 经 `meta.VerifyReadSeeker` 尺寸/分块失配 → **永久 fail-closed 不可下载**；
// （b）新落盘文件无 sidecar → 下载**静默零校验**。
//
// 为何不直接在 user 根上 Wrap：`TenantRoot` 解析器返回的是 **user 桶绝对路径**，其键为
// 裸相对路径（无功能桶段）——`meta.MetaPath` 会回落 `docs/x.meta`（用户可见目录，§8
// 记档的无桶段兜底）。故先上移到租户根（user 桶父目录）并把键映射为 `user/<rel>`，
// trusted.Wrap 随之把 sidecar 落到 `meta/<rel>.meta`——与本地卷上传路径
// `filesMetaPolicy.WriteMeta` **同一布局**，下载校验侧无需任何额外接线。
func (e *Executor) newLocalPullFS(tenantRoot string, owner string) syncpkg.FS {
	inner := e.localFS(tenantRoot, owner)
	// M8 修复（第 7 轮 MAJOR）：桶前缀视图改用 `syncpkg.PrefixFS`（**单一实现**：全能力
	// 转发——WriteIfAbsent/ReserveSpace/LocalVolume/Mover/Copier/Linker/RangeReader/
	// DirectURL/Provider/StagingQuota*，并已纳入 R21 门禁）。此前本包自建的 userBucketFS
	// 只转发 7 个基础方法：消费方对 meta.Provider/Mover 等的断言恒 false → 静默降级。
	return syncpkg.NewPrefixFS(trusted.Wrap(inner, trusted.Options{Logger: e.logger()}), "user/")
}

// buildJob 由任务参数构造 sync 引擎 Job。
func buildJob(task *syncmgr.SyncTask) *syncpkg.Job {
	return &syncpkg.Job{
		ID:             task.ID,
		Direction:      syncpkg.Direction(task.Direction),
		Src:            task.Src,
		Dst:            task.Dst,
		Recursive:      task.Recursive,
		Filters:        syncpkg.ParseFilters(task.Include, task.Exclude),
		ConflictPolicy: syncpkg.ConflictPolicy(task.ConflictPolicy),
		SyncEmptyDirs:  task.SyncEmptyDirs,
		FollowSymlinks: task.FollowSymlinks,
		Remote:         syncpkg.RemoteRef{Node: task.Remote},
		VerifyAfter:    task.VerifyAfter,
	}
}

// buildRunResult 汇总引擎结果到 syncmgr.RunResult（含载体计数上报）。
func (e *Executor) buildRunResult(job *syncpkg.Job, verifyFailed int64, srcFS, dstFS syncpkg.FS) *syncmgr.RunResult {
	return &syncmgr.RunResult{
		Status:       string(job.Status),
		FilesTotal:   job.Stats.FilesTotal,
		FilesDone:    job.Stats.FilesDone,
		BytesTotal:   job.Stats.BytesTotal,
		BytesDone:    job.Stats.BytesDone,
		FilesDeleted: job.Stats.FilesDeleted,
		VerifyFailed: verifyFailed,
		Results:      flattenResults(job.Results),
		// 载体计数（W1）：远端 FS 可选实现 CarrierReporter 时上报「实际用了直连还是中继」。
		// 双向都查（push 时远端是 dstFS，pull 时是 srcFS）；两者都没实现则留空。
		Carriers: carrierStatsOf(dstFS, srcFS),
	}
}

// applyFailureStatus 识别失败并标记可重试性：
//   - job.Status=Failed 且有 syncErr：业务/传输错误按 IsRetryableError 判定重试性；
//   - 引擎把单文件传输错误吞为 FileResult{ActionError} 且全部文件失败（网络类）：标记
//     StatusFailed + 可重试（审查 I-2，交给 syncmgr 自动重试）。
func (e *Executor) applyFailureStatus(result *syncmgr.RunResult, job *syncpkg.Job, syncErr error) {
	if job.Status == syncpkg.StatusFailed && syncErr != nil {
		result.Error = syncErr.Error()
		// 阶段 6 自动重试：瞬时网络错误（连接拒绝/超时/5xx）标记为可重试，
		// 业务失败（校验/路径等确定性错误）为 false（重试不会成功）。
		result.Retryable = httptransport.IsRetryableError(syncErr)
		return
	}
	if httptransport.IsRetryableFileFailure(job) {
		// 审查 I-2：引擎把单文件传输错误吞为 FileResult{ActionError}，最终
		// job.Status 保持 completed（不触发 StatusFailed 路径）——但"全部文件
		// 传输失败且错误为网络类"（如 push 到宕机远端）实际是可重试瞬时故障，
		// 若报 completed（0 文件 + 全部 error 结果）会误导用户。此处识别该场景
		// 并标记为可重试失败，交给 syncmgr 自动重试。
		result.Status = string(syncpkg.StatusFailed)
		result.Error = "同步全部文件传输失败（疑似瞬时网络故障，将重试）"
		result.Retryable = true
	}
}

// ErrMeshTransportNotWired 表示**本进程未注入 mesh 载体工厂**（`Executor.MeshFS == nil`），
// 故 `kind=mesh` 的远端无法建链。
//
// **不得回落 direct**：回落会让「已声明 mesh 授权」的配置静默走直连凭据，破坏授权语义
// （spec §5.7「任一载体缺其必需参数即拒，不跨载体回落」）。P3-d 起 mesh 载体本身已可实现
// （`SetMeshFSFactory` 注入 pkg/remote 构造的 FS）；本错误只在装配缺省时出现。
var ErrMeshTransportNotWired = errors.New("sync: mesh 载体尚未装配（未注入 MeshFSFactory）")

// ErrVolumeNotWired 表示**本进程未注入本机卷载体工厂**（`Executor.BaidupcsFS == nil`），
// 故 `kind=volume`（含 kind=baidupcs 别名）的远端无法访问本机卷。
//
// **不得回落 direct**：回落会让「已声明本机卷」的配置静默走远程 HTTP，破坏卷寻址语义
// （与 mesh 同一 fail-closed 原则）。本错误只在装配缺省时出现（装配层启用外部卷配置
// 时应注入工厂）。
var ErrVolumeNotWired = errors.New("sync: 本机卷载体尚未装配（未注入 BaidupcsFSFactory）")

// ErrBaidupcsNotWired 是 ErrVolumeNotWired 的兼容别名（kind=baidupcs 归一为 volume 后
// 同一错误；既有测试按 errors.Is 断言 ErrBaidupcsNotWired 仍成立）。
var ErrBaidupcsNotWired = ErrVolumeNotWired

// newRemoteFS 按载体类型构造远端 sync.FS：
//   - direct   → HTTPTransport（SproxySig 认证；现状默认，零行为变更）；
//   - mesh     → 经 mesh 隧道的 pkg/remote（P3 装配，当前明确报错而**不**回落 direct）；
//   - baidupcs → 本机网盘卷 StorageFS（P4 装配，未注入工厂明确报错而**不**回落 direct）。
//
// 返回 sync.FS 而非具体类型：这正是「远程访问只有一种抽象」的落地——同步引擎只认 FS。
func (e *Executor) newRemoteFS(ctx context.Context, remote syncmgr.RemoteConfig, owner string) (syncpkg.FS, func(), error) {
	switch remote.KindOrDirect() {
	case syncmgr.RemoteKindDirect:
		tr, err := e.newDirectTransport(remote)
		if err != nil {
			return nil, nil, err
		}
		return tr, func() { _ = tr.Close() }, nil
	case syncmgr.RemoteKindMesh:
		return e.newMeshFS(ctx, remote)
	case syncmgr.RemoteKindVolume:
		return e.newVolumeFS(ctx, remote, owner)
	default:
		return nil, nil, fmt.Errorf("remote %q: 未知载体类型 %q（可选：direct|mesh|volume）", remote.Name, remote.Kind)
	}
}

// newMeshFS 经 mesh 隧道构造远端 sync.FS（P3 装配，未注入工厂明确报错而**不**回落 direct）。
func (e *Executor) newMeshFS(ctx context.Context, remote syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
	if e.MeshFS == nil {
		return nil, nil, fmt.Errorf("remote %q: %w", remote.Name, ErrMeshTransportNotWired)
	}
	fs, closeFn, err := e.MeshFS(ctx, remote)
	if err != nil {
		// 工厂错误原样上抛（errors.Is 可判定），**绝不回落 direct**。
		return nil, nil, fmt.Errorf("remote %q: mesh 载体建链失败: %w", remote.Name, err)
	}
	if fs == nil {
		return nil, nil, fmt.Errorf("remote %q: mesh 载体工厂返回空 FS（装配错误）", remote.Name)
	}
	if closeFn == nil {
		closeFn = func() { /* 无清理 */ }
	}
	return fs, closeFn, nil
}

// newVolumeFS 构造本机卷版远端 sync.FS（kind=baidupcs 经 KindOrDirect 归一为 volume）。
// 未注入工厂明确报错而**不**回落 direct。
func (e *Executor) newVolumeFS(ctx context.Context, remote syncmgr.RemoteConfig, owner string) (syncpkg.FS, func(), error) {
	if e.BaidupcsFS == nil {
		return nil, nil, fmt.Errorf("remote %q: %w", remote.Name, ErrVolumeNotWired)
	}
	fs, closeFn, err := e.BaidupcsFS(ctx, remote, owner)
	if err != nil {
		// 工厂错误原样上抛（errors.Is 可判定），**绝不回落 direct**。
		return nil, nil, fmt.Errorf("remote %q: 本机卷建链失败: %w", remote.Name, err)
	}
	if fs == nil {
		return nil, nil, fmt.Errorf("remote %q: 本机卷工厂返回空 FS（装配错误）", remote.Name)
	}
	if closeFn == nil {
		closeFn = func() { /* 无清理 */ }
	}
	return fs, closeFn, nil
}

// newDirectTransport 构造直连远程 sproxy 的 HTTPTransport（SproxySig 认证）。
// Dial = net.Dial 到 remote URL 的 host:port。
func (e *Executor) newDirectTransport(remote syncmgr.RemoteConfig) (*httptransport.HTTPTransport, error) {
	u, err := url.Parse(remote.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("remote %q URL 非法: %q", remote.Name, remote.URL)
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", u.Host)
	}
	return httptransport.NewHTTPTransport(httptransport.HTTPTransportConfig{
		BaseURL:         remote.URL,
		Dial:            dial,
		AccessKey:       remote.AccessKey,
		AccessKeySecret: remote.AccessKeySecret,
		AccessKeyID:     remote.AccessKeyID,
		Logger:          e.logger(),
	})
}

// carrierStatsOf 从若干 FS 中取载体计数：取首个**实现了 CarrierReporter 且上报非空**的实现
// （push 的远端是 dstFS、pull 的是 srcFS；本地侧不实现）。
func carrierStatsOf(fss ...syncpkg.FS) map[string]int {
	for _, fs := range fss {
		if fs == nil {
			continue
		}
		rep, ok := fs.(CarrierReporter)
		if !ok {
			continue
		}
		if stats := rep.CarrierStats(); len(stats) > 0 {
			return stats
		}
	}
	return nil
}

// flattenResults 把 pkg/sync.FileResult 扁平化为 syncmgr.SyncFileResult。
func flattenResults(rs []syncpkg.FileResult) []syncmgr.SyncFileResult {
	if len(rs) == 0 {
		return nil
	}
	out := make([]syncmgr.SyncFileResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, syncmgr.SyncFileResult{
			Path:     r.Path,
			Action:   string(r.Action),
			Error:    r.Error,
			Size:     r.Size,
			MTime:    r.MTime,
			Checksum: r.Checksum,
		})
	}
	return out
}

// verifyAfterSync 在 job.VerifyAfter 开启时执行校验核对，返回校验失败数；
// 关闭时零开销（不调用 Verify）。
// src/dst 是校验读取侧（push：本地源+远程目标；pull：远程源+本地目标）——
// Verify 只读目标侧 Stat/checksum，比对 job.Results 里记录的源 checksum。
func (e *Executor) verifyAfterSync(ctx context.Context, src, dst syncpkg.FS, job *syncpkg.Job) int64 {
	if !job.VerifyAfter {
		return 0
	}
	failures, err := syncpkg.Verify(ctx, src, dst, job)
	if err != nil {
		// 校验过程被取消/读取失败：不把同步标失败，但记录校验失败数（含读取错误项）。
		e.logger().Warn("校验核对中断", "error", err, "failures", len(failures))
	}
	if len(failures) == 0 {
		return 0
	}
	// 校验失败结果并入 job.Results（消费方可直接展示 verify_failed 清单）。
	job.Results = append(job.Results, failures...)
	return int64(len(failures))
}

var _ syncmgr.Executor = (*Executor)(nil)

// runBoth 执行双向同步：push（本地→远程）+ pull（远程→本地）两次单向，合并进度/结果。
// push 方向本地配额不预留（对齐单向 push）；pull 方向本地写侧用 quotaLocalFS 装饰（对齐单向 pull）。
// 顺序先 push 后 pull：先把我方新增/修改推出去，再拉取对方新增/修改——两边最终一致。
func (e *Executor) runBoth(ctx context.Context, task *syncmgr.SyncTask, remote syncmgr.RemoteConfig, localRoot, tenantRoot string) (*syncmgr.RunResult, error) {
	remoteFS, closeRemote, err := e.newRemoteFS(ctx, remote, task.Owner)
	if err != nil {
		return nil, err
	}
	defer closeRemote()

	// 阶段 1：push 本地 → 远程。
	pushJob := &syncpkg.Job{
		ID:             task.ID,
		Direction:      syncpkg.DirectionPush,
		Src:            task.Src,
		Dst:            task.Dst,
		Recursive:      task.Recursive,
		Filters:        syncpkg.ParseFilters(task.Include, task.Exclude),
		ConflictPolicy: syncpkg.ConflictPolicy(task.ConflictPolicy),
		SyncEmptyDirs:  task.SyncEmptyDirs,
		FollowSymlinks: task.FollowSymlinks,
		DeletePolicy:   syncpkg.DeletePolicy(task.DeletePolicy),
		Remote:         syncpkg.RemoteRef{Node: task.Remote},
		VerifyAfter:    task.VerifyAfter,
	}
	engine := &syncpkg.Engine{Logger: e.logger(), ConflictRecorder: e.conflictRecorder()}
	pushErr := engine.Sync(ctx, e.pushSourceFS(tenantRoot, task.Owner), remoteFS, pushJob)
	if ctx.Err() != nil {
		return &syncmgr.RunResult{Status: string(syncpkg.StatusCancelled), Error: ctx.Err().Error()}, ctx.Err()
	}

	// 阶段 2：pull 远程 → 本地。
	pullJob := &syncpkg.Job{
		ID:             task.ID,
		Direction:      syncpkg.DirectionPull,
		Src:            task.Src,
		Dst:            task.Dst,
		Recursive:      task.Recursive,
		Filters:        syncpkg.ParseFilters(task.Include, task.Exclude),
		ConflictPolicy: syncpkg.ConflictPolicy(task.ConflictPolicy),
		SyncEmptyDirs:  task.SyncEmptyDirs,
		FollowSymlinks: task.FollowSymlinks,
		DeletePolicy:   syncpkg.DeletePolicy(task.DeletePolicy),
		Remote:         syncpkg.RemoteRef{Node: task.Remote},
	}
	// pull 写侧配额感知（对齐单向 pull）；本地目标叠 trusted.Wrap 生成/联动 sidecar。
	pullJobDst := &quotaLocalFS{
		inner:    e.newLocalPullFS(tenantRoot, task.Owner),
		owner:    task.Owner,
		scopeFor: e.scopeFor,
		phys:     e.physFor(task.Owner),
	}
	pullErr := engine.Sync(ctx, remoteFS, pullJobDst, pullJob)
	if ctx.Err() != nil {
		return &syncmgr.RunResult{Status: string(syncpkg.StatusCancelled), Error: ctx.Err().Error()}, ctx.Err()
	}

	// 校验核对（verify_after=true）：push 段验远程目标、pull 段验本地目标（读侧同步用源 FS）。
	pushVerify := e.verifyAfterSync(ctx, e.pushSourceFS(tenantRoot, task.Owner), remoteFS, pushJob)
	pullVerify := e.verifyAfterSync(ctx, remoteFS, pullJobDst.inner, pullJob)

	// 合并结果：进度累计（push 的传输 + pull 的传输 + 两段删除）。
	filesTotal := pushJob.Stats.FilesTotal + pullJob.Stats.FilesTotal
	filesDone := pushJob.Stats.FilesDone + pullJob.Stats.FilesDone
	bytesTotal := pushJob.Stats.BytesTotal + pullJob.Stats.BytesTotal
	bytesDone := pushJob.Stats.BytesDone + pullJob.Stats.BytesDone
	filesDeleted := pushJob.Stats.FilesDeleted + pullJob.Stats.FilesDeleted
	results := append(append([]syncpkg.FileResult(nil), pushJob.Results...), pullJob.Results...)

	status := string(syncpkg.StatusCompleted)
	var syncErr error
	if pushErr != nil {
		syncErr = pushErr
		status = string(syncpkg.StatusFailed)
	}
	if pullErr != nil {
		syncErr = pullErr
		status = string(syncpkg.StatusFailed)
	}
	result := &syncmgr.RunResult{
		Status:       status,
		FilesTotal:   filesTotal,
		FilesDone:    filesDone,
		BytesTotal:   bytesTotal,
		BytesDone:    bytesDone,
		FilesDeleted: filesDeleted,
		VerifyFailed: pushVerify + pullVerify,
		Results:      flattenResults(results),
		Carriers:     carrierStatsOf(remoteFS),
	}
	if status == string(syncpkg.StatusFailed) && syncErr != nil {
		result.Error = syncErr.Error()
		result.Retryable = httptransport.IsRetryableError(syncErr)
	}
	return result, syncErr
}

// ListDirNames 转发轻量列举（未实现时回落 ListDir）：装配期只按名字分类时避免外部卷
// 逐条补精确 size（见 sync.NameLister）。
func (q *quotaLocalFS) ListDirNames(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if nl, ok := syncpkg.AssertNameLister(q.inner); ok {
		return nl.ListDirNames(ctx, p)
	}
	return q.inner.ListDir(ctx, p)
}

var (
	_ syncpkg.NameLister = (*quotaLocalFS)(nil)
)
