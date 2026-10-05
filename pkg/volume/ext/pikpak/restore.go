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
// 会话约束（F9）：Release 逐账号在**对应账号会话内**执行（round-13 修正：多账号下载的
// 副本分属 N 个账号，单会话批量删只会删到最后一个会话账号的副本，其余永久泄漏占空间——
// 账号池路径经 pool.Use 逐账号切会话删除；单账号/当前会话路径直接删）。旧下载器把
// Release 包在自己的 Use 块内调用，构造时 pool 传 nil（避免嵌套 Use 死锁，当前会话即账号）。
type RestoreLease struct {
	api        *API
	pool       *AccountPool // 可选：多账号时按账号会话逐账号释放（nil = 当前会话/单账号）
	log        *slog.Logger
	autoDelete bool

	mu     sync.Mutex
	byAcct map[string][]string // 账号名 → 转存副本 ID（"" = 当前会话/单账号）
}

// NewRestoreLease 创建转存副本释放租赁（每下载调用独立实例，不共享）。
func NewRestoreLease(api *API, pool *AccountPool, autoDelete bool, log *slog.Logger) *RestoreLease {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &RestoreLease{api: api, pool: pool, log: log, autoDelete: autoDelete, byAcct: map[string][]string{}}
}

// Track 记录本次转存的 restore 副本 ID（当前会话/单账号；AutoDelete 时 Release 永久删除）。
func (l *RestoreLease) Track(id string) { l.TrackIn("", id) }

// TrackIn 记录某账号会话内的转存副本（多账号路径按账号登记，Release 按账号逐会话删除）。
func (l *RestoreLease) TrackIn(acct, id string) {
	if id == "" || !l.autoDelete {
		return
	}
	l.mu.Lock()
	l.byAcct[acct] = append(l.byAcct[acct], id)
	l.mu.Unlock()
}

// HasTracked 是否已记录副本（hybrid 缓存路径复用判断）。
func (l *RestoreLease) HasTracked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byAcct) > 0
}

// LastID 返回最近记录的副本 ID（hybrid 缓存路径取用；无记录返回 ""）。
// 语义沿用单账号路径：取当前会话组（""）最近一条。
func (l *RestoreLease) LastID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := l.byAcct[""]
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

// Release 统一释放：AutoDelete 时永久删除全部记录的副本（单一删除语义，DeletePermanent）。
// 多账号副本按账号分组、逐账号在对应 Use 会话内删除（F9——sessionMu 串行保证每账号删除
// 都用自己的 token）；池为 nil 或账号名为空时用当前会话直接删。幂等：已释放后再次调用
// 为 no-op——下载器多条完成/失败路径可安全共用。
func (l *RestoreLease) Release(ctx context.Context) {
	if !l.autoDelete {
		return
	}
	l.mu.Lock()
	by := l.byAcct
	l.byAcct = map[string][]string{}
	l.mu.Unlock()
	if len(by) == 0 {
		return
	}
	for acct, ids := range by {
		// 账号池路径：该账号副本必须在该账号会话内删（并发 Use 已把会话切走/或将切走）。
		if l.pool == nil || acct == "" {
			if err := l.deleteIn(ctx, "" /* 当前会话 */, ids); err != nil {
				l.log.Warn("restore lease release failed", "acct", acct, "ids", ids, "err", err)
			}
			continue
		}
		if err := l.pool.Use(ctx, acct, func() error { return l.deleteIn(ctx, acct, ids) }); err != nil {
			l.log.Warn("restore lease release (account) failed", "acct", acct, "ids", ids, "err", err)
		}
	}
}

// deleteIn 在当前会话下永久删除给定副本（ResetToken 让 REST 从当前会话凭据文件重读 token）。
func (l *RestoreLease) deleteIn(ctx context.Context, acct string, ids []string) error {
	l.api.ResetToken() // 会话已切到 acct（由调用方在 Use 内调用）或当前登录态
	if err := l.api.DeletePermanent(ctx, ids); err != nil {
		return err
	}
	l.log.Info("restore lease released (permanent)", "acct", acct, "count", len(ids))
	return nil
}
