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

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/sync/httptransport"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// Executor 基于 pkg/sync.Engine 的同步执行器（实现 syncmgr.Executor）。
type Executor struct {
	// TenantRoot 按任务 owner 解析租户 user 根绝对路径（push 的 src / pull 的 dst 根）。
	// 装配层注入（如 Handlers.syncTenantRoot）；nil 时 Run 报错（fail-closed）。
	TenantRoot syncmgr.TenantRootResolver
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
	// 与 mesh 同构：`pkg/syncexec` 不得依赖 `pkg/baidupcs` 具体类型、也不自己装配网盘卷
	// （卷名/凭据/中间态目录全在装配层）。工厂按远端配置的 Volume（本机卷名）构造对应
	// StorageFS 并返回 close。
	//
	// nil = 未装配 baidupcs 载体：`kind=baidupcs` 的远端明确报 ErrBaidupcsNotWired（**绝不
	// 回落 direct**——回落会让「已声明本机卷」的配置静默走远程 HTTP，破坏卷寻址语义）。
	BaidupcsFS BaidupcsFSFactory
}

// MeshFSFactory 按远端配置构造 mesh 版 `sync.FS`，并返回任务结束时调用的 close（关链路）。
type MeshFSFactory func(ctx context.Context, remote syncmgr.RemoteConfig) (syncpkg.FS, func(), error)

// BaidupcsFSFactory 按远端配置构造本机百度网盘卷版 `sync.FS`，并返回任务结束时调用的
// close（关链路）。签名与 MeshFSFactory 相同（调用方只消费 sync.FS 抽象）。
type BaidupcsFSFactory func(ctx context.Context, remote syncmgr.RemoteConfig) (syncpkg.FS, func(), error)

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
func (q *quotaLocalFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	scope := q.examScope(relPath)
	if scope == nil {
		return q.inner.WriteFile(ctx, relPath, r, size, mtime)
	}
	res, err := scope.TryReserve(size)
	if err != nil {
		return err // 配额不足：该文件失败（ActionError），不中止整体同步
	}
	// 委托 inner.WriteFile（其 copyWithCtx 已做 ctx 感知拷贝）。成功 Commit(size)——
	// 源条目 size 即已知真实大小（与 engine 的 BytesDone/进度一致）；失败 Release 归还。
	if werr := q.inner.WriteFile(ctx, relPath, r, size, mtime); werr != nil {
		res.Release()
		return werr
	}
	res.Commit(size)
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
	var srcFS, dstFS syncpkg.FS
	var remoteClosers []func()
	defer func() {
		for _, closeFn := range remoteClosers {
			if closeFn != nil {
				closeFn()
			}
		}
	}()

	// 本地端（push 的 src / pull 的 dst）按任务 owner 解析到租户 user 根（<root>/<tenant>/user）。
	// 布局迁移后由注入的 TenantRoot 解析器派生（与 pkg/server 租户布局单一来源，owner
	// 校验集中到 pkg/storage，消除双端漂移）；owner 由服务端派生（可信），解析失败 fail-closed。
	localRoot, err := e.userRootFor(task.Owner)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return nil, fmt.Errorf("创建本地同步根 %s 失败: %w", localRoot, err)
	}

	if task.Direction == string(syncmgr.DirectionPush) {
		srcFS = syncpkg.NewLocalFS(localRoot, e.logger())
		remoteFS, closeRemote, err := e.newRemoteFS(ctx, remote)
		if err != nil {
			return nil, err
		}
		remoteClosers = append(remoteClosers, closeRemote)
		dstFS = remoteFS
	} else {
		remoteFS, closeRemote, err := e.newRemoteFS(ctx, remote)
		if err != nil {
			return nil, err
		}
		remoteClosers = append(remoteClosers, closeRemote)
		srcFS = remoteFS
		dstFS = &quotaLocalFS{
			inner: syncpkg.NewLocalFS(localRoot, e.logger()),
			owner: task.Owner,
			// 解析器按 (owner, rel) 路由 bucket_limits 子目录配额；e.tenantScope 仅按 owner
			// 取 user 桶——由 syncexec 统一补 "user/" 前缀交给注册的 scopeFor（若为 nil 则
			// 该租户无配额，退化为直写）。装配层注入的 ParseScope 见 Handlers.SyncQuotaScope。
			scopeFor: e.scopeFor,
		}
	}

	job := &syncpkg.Job{
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
	}

	engine := &syncpkg.Engine{Logger: e.logger()}
	syncErr := engine.Sync(ctx, srcFS, dstFS, job)

	result := &syncmgr.RunResult{
		Status:     string(job.Status),
		FilesTotal: job.Stats.FilesTotal,
		FilesDone:  job.Stats.FilesDone,
		BytesTotal: job.Stats.BytesTotal,
		BytesDone:  job.Stats.BytesDone,
		Results:    flattenResults(job.Results),
		// 载体计数（W1）：远端 FS 可选实现 CarrierReporter 时上报「实际用了直连还是中继」。
		// 双向都查（push 时远端是 dstFS，pull 时是 srcFS）；两者都没实现则留空。
		Carriers: carrierStatsOf(dstFS, srcFS),
	}
	if job.Status == syncpkg.StatusFailed && syncErr != nil {
		result.Error = syncErr.Error()
		// 阶段 6 自动重试：瞬时网络错误（连接拒绝/超时/5xx）标记为可重试，
		// 业务失败（校验/路径等确定性错误）为 false（重试不会成功）。
		result.Retryable = httptransport.IsRetryableError(syncErr)
	} else if httptransport.IsRetryableFileFailure(job) {
		// 审查 I-2：引擎把单文件传输错误吞为 FileResult{ActionError}，最终
		// job.Status 保持 completed（不触发 StatusFailed 路径）——但"全部文件
		// 传输失败且错误为网络类"（如 push 到宕机远端）实际是可重试瞬时故障，
		// 若报 completed（0 文件 + 全部 error 结果）会误导用户。此处识别该场景
		// 并标记为可重试失败，交给 syncmgr 自动重试。
		result.Status = string(syncpkg.StatusFailed)
		result.Error = "同步全部文件传输失败（疑似瞬时网络故障，将重试）"
		result.Retryable = true
	}
	return result, syncErr
}

// ErrMeshTransportNotWired 表示**本进程未注入 mesh 载体工厂**（`Executor.MeshFS == nil`），
// 故 `kind=mesh` 的远端无法建链。
//
// **不得回落 direct**：回落会让「已声明 mesh 授权」的配置静默走直连凭据，破坏授权语义
// （spec §5.7「任一载体缺其必需参数即拒，不跨载体回落」）。P3-d 起 mesh 载体本身已可实现
// （`SetMeshFSFactory` 注入 pkg/remote 构造的 FS）；本错误只在装配缺省时出现。
var ErrMeshTransportNotWired = errors.New("sync: mesh 载体尚未装配（未注入 MeshFSFactory）")

// ErrBaidupcsNotWired 表示**本进程未注入 baidupcs 载体工厂**（`Executor.BaidupcsFS == nil`），
// 故 `kind=baidupcs` 的远端无法访问本机网盘卷。
//
// **不得回落 direct**：回落会让「已声明本机卷」的配置静默走远程 HTTP，破坏卷寻址语义
// （与 mesh 同一 fail-closed 原则）。本错误只在装配缺省时出现（装配层启用 baidupcs 配置
// 时应注入工厂）。
var ErrBaidupcsNotWired = errors.New("sync: baidupcs 载体尚未装配（未注入 BaidupcsFSFactory）")

// newRemoteFS 按载体类型构造远端 sync.FS：
//   - direct   → HTTPTransport（SproxySig 认证；现状默认，零行为变更）；
//   - mesh     → 经 mesh 隧道的 pkg/remote（P3 装配，当前明确报错而**不**回落 direct）；
//   - baidupcs → 本机网盘卷 StorageFS（P4 装配，未注入工厂明确报错而**不**回落 direct）。
//
// 返回 sync.FS 而非具体类型：这正是「远程访问只有一种抽象」的落地——同步引擎只认 FS。
func (e *Executor) newRemoteFS(ctx context.Context, remote syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
	switch remote.KindOrDirect() {
	case syncmgr.RemoteKindDirect:
		tr, err := e.newDirectTransport(remote)
		if err != nil {
			return nil, nil, err
		}
		return tr, func() { _ = tr.Close() }, nil
	case syncmgr.RemoteKindMesh:
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
			closeFn = func() {}
		}
		return fs, closeFn, nil
	case syncmgr.RemoteKindBaidupcs:
		if e.BaidupcsFS == nil {
			return nil, nil, fmt.Errorf("remote %q: %w", remote.Name, ErrBaidupcsNotWired)
		}
		fs, closeFn, err := e.BaidupcsFS(ctx, remote)
		if err != nil {
			// 工厂错误原样上抛（errors.Is 可判定），**绝不回落 direct**。
			return nil, nil, fmt.Errorf("remote %q: baidupcs 载体建链失败: %w", remote.Name, err)
		}
		if fs == nil {
			return nil, nil, fmt.Errorf("remote %q: baidupcs 载体工厂返回空 FS（装配错误）", remote.Name)
		}
		if closeFn == nil {
			closeFn = func() {}
		}
		return fs, closeFn, nil
	default:
		return nil, nil, fmt.Errorf("remote %q: 未知载体类型 %q（可选：direct|mesh|baidupcs）", remote.Name, remote.Kind)
	}
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

var _ syncmgr.Executor = (*Executor)(nil)
