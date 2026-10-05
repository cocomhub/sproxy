// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// RestoreLease 收归「转存副本释放」单一实现（2026-10-05 用户裁决：
// 通过机制架构避免问题——释放流程单一处理，避免多个使用方重复实现）。
//
// 任何下载器（hybrid / 旧 PikpakDownloader / 未来扩展）转存后把 restore 副本 ID 记入
// lease，下载完成/失败时统一调 Release 永久删除。owned（file_restore_own：源文件已在
// 网盘）与复用的用户文件一律不 Track（NH-P1：绝不误删用户数据）。
//
// 删除语义单一（DeletePermanent 释放配额空间）——旧下载器此前用 batchTrash 只移回收站
// 不释放空间的缺陷一并收口。AutoDelete=false 时全部 no-op。
//
// 会话约束（F9）：调用方负责在**选中账号会话内**调 Release（账号池路径须在 Use 块内）。
type RestoreLease struct {
	api        *API
	log        *slog.Logger
	autoDelete bool

	mu  sync.Mutex
	ids []string
}

// NewRestoreLease 创建转存副本释放租赁（每下载调用独立实例，不共享）。
func NewRestoreLease(api *API, autoDelete bool, log *slog.Logger) *RestoreLease {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &RestoreLease{api: api, log: log, autoDelete: autoDelete}
}

// Track 记录本次转存的 restore 副本 ID（AutoDelete 时 Release 会永久删除）。
func (l *RestoreLease) Track(id string) {
	if id == "" || !l.autoDelete {
		return
	}
	l.mu.Lock()
	l.ids = append(l.ids, id)
	l.mu.Unlock()
}

// HasTracked 是否已记录副本（hybrid 缓存路径复用判断）。
func (l *RestoreLease) HasTracked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ids) > 0
}

// LastID 返回最近记录的副本 ID（hybrid 缓存路径取用；无记录返回 ""）。
func (l *RestoreLease) LastID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ids) == 0 {
		return ""
	}
	return l.ids[len(l.ids)-1]
}

// Release 统一释放：AutoDelete 时永久删除全部记录的副本（单一删除语义）。
// 幂等：已释放（ids 清空）后再次调用为 no-op——下载器多条完成/失败路径可安全共用。
func (l *RestoreLease) Release(ctx context.Context) {
	if !l.autoDelete {
		return
	}
	l.mu.Lock()
	ids := l.ids
	l.ids = nil
	l.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	if err := l.api.DeletePermanent(ctx, ids); err != nil {
		l.log.Warn("restore lease release failed", "ids", ids, "err", err)
	} else {
		l.log.Info("restore lease released (permanent)", "count", len(ids))
	}
}
