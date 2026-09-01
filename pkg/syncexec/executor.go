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
	"log/slog"
	"net"
	"net/url"
	"os"

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// Executor 基于 pkg/sync.Engine 的同步执行器（实现 syncmgr.Executor）。
type Executor struct {
	// UploadsDir 是本地 FS 根（push 的 src / pull 的 dst 的基础目录）。
	// 实际本地根按任务 owner 派生为 <UploadsDir>/<owner>（多租户隔离，审查 F1）。
	UploadsDir string
	// Logger 是执行日志。
	Logger *slog.Logger
}

// NewExecutor 创建执行器。
func NewExecutor(uploadsDir string, logger *slog.Logger) *Executor {
	return &Executor{UploadsDir: uploadsDir, Logger: logger}
}

func (e *Executor) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
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

	// 本地端（push 的 src / pull 的 dst）按任务 owner 隔离到 <uploadsDir>/<owner>。
	// 与 pkg/server uploadsDir/<owner>/ 布局一致（审查 F1）：owner 为空回落全局根。
	// owner 由服务端派生（可信），OwnerFileRoot 仍做纵深校验。
	localRoot := syncmgr.OwnerFileRoot(e.UploadsDir, task.Owner)
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
		dstFS = syncpkg.NewLocalFS(localRoot, e.logger())
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
