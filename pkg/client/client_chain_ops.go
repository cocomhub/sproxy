// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// client_chain_ops.go 是 SDK 的**链式操作入口**：一键云下载链（CloudDownloadChain）、
// 组链（CloudDownloadGroupChain）、续跑（ResumeChain）、列举与删除链记录，以及 ChainState 摘要。
//
// 拆分说明见 client.go 顶部。

package client

import (
	"context"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
)

func (c *FileClient) CloudDownloadChain(ctx context.Context,
	urls []string, archiveName, localDir string,
	opts ...ChainOption) (*ChainResult, error) {
	options := defaultChainOptions()
	for _, o := range opts {
		o(&options)
	}

	runner, err := NewCloudDownloadChain(c, urls, archiveName, localDir, options)
	if err != nil {
		return nil, fmt.Errorf("创建云端下载链失败: %w", err)
	}

	if c.chainManager != nil {
		if err := c.chainManager.RunWithProgress(ctx, runner, options.progressFn); err != nil {
			return nil, err
		}
	} else {
		reportFn := func(ctx context.Context, info ProgressInfo) {
			c.logger.DebugContext(ctx, "链式操作进度", "phase", info.Phase, "msg", info.Message, "current", info.Current, "total", info.Total)
			if options.progressFn != nil {
				options.progressFn(ctx, info)
			}
		}
		if err := runner.Run(ctx, reportFn); err != nil {
			return nil, err
		}
	}

	return &ChainResult{
		ChainID: runner.ChainID,
		Phase:   runner.Phase(),
		Status:  runner.Status(),
		raw:     runner,
		extra: map[string]any{
			"local_path": runner.LocalPath,
			"keep_files": runner.KeepFiles,
		},
	}, nil
}

// CloudDownloadGroupChain 云端组下载一键链式操作：创建组 → 等待完成 → 打包 → 下载到本地 → 清理远端组。
// 与 CloudDownloadChain 不同：提交阶段创建命名组（文件名冲突预检在调用方做），
// 等待阶段轮询组状态，归档阶段调组级打包，清理阶段删除整个组。
func (c *FileClient) CloudDownloadGroupChain(ctx context.Context,
	groupName string, entries []cloudfilename.Entry, archiveName, localDir string,
	opts ...ChainOption) (*ChainResult, error) {
	options := defaultChainOptions()
	for _, o := range opts {
		o(&options)
	}

	runner, err := NewCloudDownloadGroupChain(c, groupName, entries, archiveName, localDir, options)
	if err != nil {
		return nil, fmt.Errorf("创建云端组下载链失败: %w", err)
	}

	if c.chainManager != nil {
		if err := c.chainManager.RunWithProgress(ctx, runner, options.progressFn); err != nil {
			return nil, err
		}
	} else {
		reportFn := func(ctx context.Context, info ProgressInfo) {
			c.logger.DebugContext(ctx, "链式操作进度", "phase", info.Phase, "msg", info.Message, "current", info.Current, "total", info.Total)
			if options.progressFn != nil {
				options.progressFn(ctx, info)
			}
		}
		if err := runner.Run(ctx, reportFn); err != nil {
			return nil, err
		}
	}

	extra := map[string]any{
		"local_path": runner.LocalPath,
		"keep_files": runner.KeepFiles,
	}
	if runner.GroupID != "" {
		extra["group_id"] = runner.GroupID
	}
	return &ChainResult{
		ChainID: runner.ChainID,
		Phase:   runner.Phase(),
		Status:  runner.Status(),
		raw:     runner,
		extra:   extra,
	}, nil
}

// ResumeChain 从缓存恢复链式操作。
func (c *FileClient) ResumeChain(ctx context.Context, chainID string) (*ChainResult, error) {
	if c.chainManager == nil {
		return nil, fmt.Errorf("链式操作未启用持久化，请使用 WithCacheDir 或 WithKVStore 创建客户端")
	}

	runner, err := c.chainManager.Resume(ctx, chainID)
	if err != nil {
		return nil, err
	}

	runner.SetClient(c)
	opts := chainOptions{
		pollInterval: 3 * time.Second,
		timeout:      30 * time.Minute,
		keepFiles:    false,
	}
	if cdc, ok := runner.(*CloudDownloadChain); ok {
		if cdc.PollInterval > 0 {
			opts.pollInterval = cdc.PollInterval
		}
		if cdc.Timeout > 0 {
			opts.timeout = cdc.Timeout
		}
		opts.keepFiles = cdc.KeepFiles
	}
	// CloudDownloadGroupChain 恢复同样需要取回持久化的轮询/超时/keepFiles 选项，
	// 否则中断的组链式操作恢复后会退回默认值（3s/30m/false）。
	if gdc, ok := runner.(*CloudDownloadGroupChain); ok {
		if gdc.PollInterval > 0 {
			opts.pollInterval = gdc.PollInterval
		}
		if gdc.Timeout > 0 {
			opts.timeout = gdc.Timeout
		}
		opts.keepFiles = gdc.KeepFiles
	}
	runner.SetOptions(opts)

	if err := c.chainManager.Run(ctx, runner); err != nil {
		return nil, err
	}

	extra := map[string]any{}
	if cdc, ok := runner.(*CloudDownloadChain); ok {
		extra["local_path"] = cdc.LocalPath
		extra["keep_files"] = cdc.KeepFiles
	}
	if gdc, ok := runner.(*CloudDownloadGroupChain); ok {
		extra["local_path"] = gdc.LocalPath
		extra["keep_files"] = gdc.KeepFiles
		if gdc.GroupID != "" {
			extra["group_id"] = gdc.GroupID
		}
	}

	return &ChainResult{
		ChainID: runner.ID(),
		Phase:   runner.Phase(),
		Status:  runner.Status(),
		raw:     runner,
		extra:   extra,
	}, nil
}

// ListChains 列出所有活跃链式操作。
func (c *FileClient) ListChains(ctx context.Context) ([]*ChainState, error) {
	if c.chainManager == nil {
		return nil, nil
	}
	runners, err := c.chainManager.List(ctx)
	if err != nil {
		return nil, err
	}
	var states []*ChainState
	for _, r := range runners {
		states = append(states, &ChainState{
			ChainID: r.ID(),
			Phase:   r.Phase(),
			Status:  r.Status(),
		})
	}
	return states, nil
}

// DeleteChain 删除链式操作缓存。
func (c *FileClient) DeleteChain(ctx context.Context, chainID string) error {
	if c.chainManager == nil {
		return nil
	}
	return c.chainManager.Delete(ctx, chainID)
}

// ChainState 链式操作摘要状态。
type ChainState struct {
	ChainID string `json:"chain_id"`
	Phase   string `json:"phase"`
	Status  string `json:"status"`
}
