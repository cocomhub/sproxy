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

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// Executor 基于 pkg/sync.Engine 的同步执行器（实现 syncmgr.Executor）。
type Executor struct {
	// UploadsDir 是本地 FS 根（push 的 src / pull 的 dst）。
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

	if task.Direction == string(syncmgr.DirectionPush) {
		srcFS = syncpkg.NewLocalFS(e.UploadsDir, e.logger())
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
		dstFS = syncpkg.NewLocalFS(e.UploadsDir, e.logger())
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
