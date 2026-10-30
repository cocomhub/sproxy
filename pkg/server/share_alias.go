// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// share_alias.go 是**迁移期兼容层**：分享存储实现已下沉 pkg/server/share，
// 装配层 handler 与存量测试用别名保持零改动。移除条件：分享 HTTP 面整体搬入切片后删除。
package server

import (
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/server/share"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
)

type (
	// ShareStore 是进程内分享存储（带持久化）。
	ShareStore = share.Store
	// ShareLink 是一条分享链接。
	ShareLink = share.Link
	// ShareStoreIface 是分享存储接口（进程内 / StateStore 两种实现）。
	ShareStoreIface = share.StoreIface
	// ShareCreateResponse 是创建分享的响应 DTO。
	ShareCreateResponse = share.CreateResponse
	// 迁移期别名（测试构造用）。
	shareSpec             = share.Spec
	shareLinkPersist      = share.LinkPersist
	stateBackedShareStore = share.StateBackedStore
)

const (
	// 分享域上限（与 pkg/server/share 同源）。
	maxShareTTL      = share.MaxShareTTL
	maxShareEntries  = share.MaxShareEntries
	maxShareBodySize = share.MaxShareBodySize
)

// NewShareStore 构造进程内分享存储。
func NewShareStore(logger *slog.Logger) *share.Store { return share.NewStore(logger) }

// newStateBackedShareStore 构造 StateStore 后端的分享存储。
func newStateBackedShareStore(st state.StateStore, legacyRoot *storage.Root, legacyRel string, logger *slog.Logger) *share.StateBackedStore {
	return share.NewStateBackedStore(st, legacyRoot, legacyRel, logger)
}
