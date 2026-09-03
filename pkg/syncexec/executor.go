// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package syncexec 提供 syncmgr.Executor 的基于 pkg/sync 引擎的实现。
//
// 模块边界：pkg/server（经 syncmgr）不依赖 pkg/sync（其 HTTPTransport 依赖 pkg/client，
// 会与 pkg/client 的 e2e 测试形成 import cycle）。因此实际同步执行放在本包，由
// cmd/sproxy 装配注入 syncmgr.Manager。
package syncexec

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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
}

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
	var httpTransports []*syncpkg.HTTPTransport
	cleanupTransports := func() {
		for _, tr := range httpTransports {
			_ = tr.Close()
		}
	}
	defer cleanupTransports()

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
		tr, err := e.newRemoteTransport(remote)
		if err != nil {
			return nil, err
		}
		httpTransports = append(httpTransports, tr)
		dstFS = tr
	} else {
		tr, err := e.newRemoteTransport(remote)
		if err != nil {
			return nil, err
		}
		httpTransports = append(httpTransports, tr)
		srcFS = tr
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
	}
	if job.Status == syncpkg.StatusFailed && syncErr != nil {
		result.Error = syncErr.Error()
		// 阶段 6 自动重试：瞬时网络错误（连接拒绝/超时/5xx）标记为可重试，
		// 业务失败（校验/路径等确定性错误）为 false（重试不会成功）。
		result.Retryable = syncpkg.IsRetryableError(syncErr)
	} else if syncpkg.IsRetryableFileFailure(job) {
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

// newRemoteTransport 构造直连远程 sproxy 的 HTTPTransport（SproxySig 认证）。
// Dial = net.Dial 到 remote URL 的 host:port。
func (e *Executor) newRemoteTransport(remote syncmgr.RemoteConfig) (*syncpkg.HTTPTransport, error) {
	u, err := url.Parse(remote.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("remote %q URL 非法: %q", remote.Name, remote.URL)
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", u.Host)
	}
	return syncpkg.NewHTTPTransport(syncpkg.HTTPTransportConfig{
		BaseURL:         remote.URL,
		Dial:            dial,
		AccessKey:       remote.AccessKey,
		AccessKeySecret: remote.AccessKeySecret,
		Logger:          e.logger(),
	})
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
