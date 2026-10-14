// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// meta_remote.go 是远程只读面的 **meta 面（op=meta）** 持有侧实现（评审 P2-10）：
// 把「用户相对路径」解析为**持有侧自身口径**的 sidecar 原始字节，供出口节点做跨节点
// 逐分块完整性校验。
//
// # 为何需要这一面
//
// 可信卷的读校验信任根是 sidecar。出口（egress）卷是**代理型卷**——它没有本地文件，
// sidecar 在持有侧的键空间里（本地卷 `meta/<rel>.meta`；外部卷由其 meta.Provider 决定）。
// 出口侧按键拼接既做不到（键空间不可知）也不应该做（会把 meta 桶语义泄露给远端）。
// 因此反过来：持有侧用**自己会用来校验下载的那份元数据**回答，出口侧无需知道任何
// 内部布局。
//
// # 与既有读校验口径的一致性（关键）
//
// 本文件刻意**镜像持有侧自身的校验口径**：
//   - 本地卷：即 `filesMetaPolicy.VerifyDownload` 所读的 `meta.MetaPath(user/<rel>)`；
//     加密卷跳过（sidecar 描述密文、服务流是明文，口径不一致——同 `VerifyDownload`）。
//   - 外部卷：即 `externalRemotePath` 给下载源注入的 `meta.Provider.FileMeta`。
//
// 因此「出口侧校验用的元数据」== 「持有侧自己下载时会校验的元数据」，不会引入新的
// 语义分叉；若两者不一致，持有侧自己的下载早就是坏的。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// readMetaForRemote 读取「用户相对路径」在持有侧对应的 sidecar 原始 JSON 字节。
//
// 返回 `*files.HTTPError{404}` 表示**持有侧无可用 sidecar**（卷未启用 meta / 存量旁路
// 写 / 加密卷口径不一致 / 文件不存在），这是预期的降级态，不是错误——远程面按 404 回，
// 出口侧据 os.ErrNotExist 分类为「无校验能力」。
func (h *Handlers) readMetaForRemote(ctx context.Context, owner, volName, remotePath string) ([]byte, error) {
	notFound := &files.HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	rel0, err := pathguard.ValidateFilePath(remotePath)
	if err != nil {
		return nil, notFound
	}
	// 本地卷：直接读租户 meta 桶（与 filesMetaPolicy.VerifyDownload 同一键）。
	if tnt := h.volumeTenant(volName, owner); tnt != nil && tnt.Root() != nil {
		return readLocalMetaForRemote(ctx, tnt, rel0, notFound)
	}
	return h.readExternalMetaForRemote(ctx, owner, volName, rel0, notFound)
}

// readLocalMetaForRemote 本地卷分支：读 `meta.MetaPath(user/<rel>)`。
func readLocalMetaForRemote(ctx context.Context, tnt *storage.Tenant, rel0 string, notFound *files.HTTPError) ([]byte, error) {
	root := tnt.Root()
	if root.IsEncrypted() {
		// 加密卷：sidecar 描述密文，而服务流是明文（入口 OpenDecrypted）——口径不一致，
		// 与 VerifyDownload 的加密卷跳过同源（完整性由卷内 GCM 认证保证）。
		return nil, notFound
	}
	rel, ok := tnt.UserRel(rel0)
	if !ok {
		return nil, notFound
	}
	raw, rerr := readMetaSidecarBytes(ctx, root, meta.MetaPath(rel))
	if errors.Is(rerr, os.ErrNotExist) {
		// 真·sidecar 缺失（存量/旁路写）→ 404（预期降级）；其它错误（存在但不可读）
		// 保持原样 → 远程面 500，出口侧归为「读取失败」告警（不静默当无校验能力）。
		return nil, notFound
	}
	return raw, rerr
}

// readExternalMetaForRemote 外部卷分支：经其 meta.Provider（与下载源注入的读取器同一
// 实例、同一键 FSPath）生成 sidecar 字节。
func (h *Handlers) readExternalMetaForRemote(ctx context.Context, owner, volName, rel0 string, notFound *files.HTTPError) ([]byte, error) {
	fsys, ok := h.externalFSFor(volName)
	if !ok || h.volSet == nil {
		return nil, notFound
	}
	v, ok := h.volSet.ByName(volName)
	if !ok {
		return nil, notFound
	}
	loc, kerr := v.ResolveUserLocation(owner, rel0)
	if kerr != nil {
		return nil, notFound
	}
	pv, ok := fsys.(meta.Provider)
	if !ok {
		return nil, notFound
	}
	fm, ferr := pv.FileMeta(ctx, loc.FSPath())
	if ferr != nil {
		if errors.Is(ferr, os.ErrNotExist) {
			return nil, notFound
		}
		return nil, ferr
	}
	raw, merr := meta.Marshal(fm)
	if merr != nil {
		return nil, fmt.Errorf("远程 meta：序列化失败: %w", merr)
	}
	return raw, nil
}

// readMetaSidecarBytes 从卷根读 sidecar 原始字节（限读上限+1 判超限，不静默截断成畸形
// JSON——与 filesMetaPolicy.VerifyDownload 同一防护口径）。
func readMetaSidecarBytes(ctx context.Context, root *storage.Root, mrel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rc, err := root.Open(mrel)
	if err != nil {
		return nil, err // 含 os.ErrNotExist（真缺失）与读取失败（调用方区分）
	}
	defer func() { _ = rc.Close() }()
	raw, rerr := io.ReadAll(io.LimitReader(rc, meta.MaxMetaSidecarBytes()+1))
	if rerr != nil {
		return nil, fmt.Errorf("远程 meta：sidecar 读取失败: %w", rerr)
	}
	if int64(len(raw)) > meta.MaxMetaSidecarBytes() {
		return nil, fmt.Errorf("远程 meta：sidecar 超过上限 %d 字节", meta.MaxMetaSidecarBytes())
	}
	return raw, nil
}
