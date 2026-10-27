// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// transfer_source_meta.go 负责把**转存来源信息**写入目标卷 meta.Extra（用户裁定
// 2026-10-27）：下游据此区分「转存产物」与「直接上传」，并溯源到源 URL / ETag。
//
// 与 recordDamagedSource（source_integrity=damaged）同机制：经目标 FS 的
// UpdateMetaExtra 旁路更新 sidecar（不重写主文件），失败 Warn 不阻断转存
// （meta 标记是增值信息，不是转存成功的前置条件）。
//
// 写入字段（合并语义，客户端 meta_extra 优先不覆盖服务端来源键）：
//
//	source          = "transfer"（固定；与 upload 来源区分）
//	source_url      = task.URL（源 URL，含 spx_target 等定位参数）
//	etag            = result.ETag（服务端唯一性标识；空则不写）
//	checksum        = result.Checksum（SHA-256；空则不写）
//	authority_hash  = result.AuthorityHash（带外权威 hash 如 pikpak GCID；空则不写）
//	transferred_at  = RFC3339 转存时刻
//	<客户端 meta_extra 各键>（同名键：客户端值优先——客户端更懂业务语义）
package cloud

import (
	"time"
)

// transferSourceMetaKeys 是服务端写入的来源键（客户端 meta_extra 同名键**不覆盖**
// 服务端来源语义，除非显式提供——见 buildTransferSourceMeta 的合并次序）。
const (
	metaKeySource        = "source"
	metaKeySourceURL     = "source_url"
	metaKeyETag          = "etag"
	metaKeyChecksum      = "checksum"
	metaKeyAuthorityHash = "authority_hash"
	metaKeyTransferredAt = "transferred_at"
)

// buildTransferSourceMeta 组装转存来源 meta（服务端来源键 + 客户端 meta_extra）。
// 次序：先服务端来源，再客户端 extra（客户端同名键覆盖服务端——业务语义优先）；
// 空值不写入（不产生 etag:"" 噪声）。
func buildTransferSourceMeta(task *CloudTask, etag, checksum, authorityHash string) map[string]any {
	meta := map[string]any{
		metaKeySource:        "transfer",
		metaKeyTransferredAt: time.Now().UTC().Format(time.RFC3339),
	}
	if task != nil && task.URL != "" {
		meta[metaKeySourceURL] = task.URL
	}
	if etag != "" {
		meta[metaKeyETag] = etag
	}
	if checksum != "" {
		meta[metaKeyChecksum] = checksum
	}
	if authorityHash != "" {
		meta[metaKeyAuthorityHash] = authorityHash
	}
	if task != nil {
		for k, v := range task.MetaExtra {
			if k == "" || v == nil {
				continue
			}
			meta[k] = v // 客户端值覆盖同名服务端键（业务语义优先）
		}
	}
	return meta
}

// recordTransferSourceMeta 把来源 meta 写入目标卷（经 UpdateMetaExtra 旁路合并）。
// 无能力（卷未实现 meta extra 更新）→ 跳过；写失败 → Warn（不阻断转存）。
func (m *CloudDownloadManager) recordTransferSourceMeta(env *transferEnv) {
	ue, ok := env.targetFS.(metaExtraUpdater)
	if !ok {
		return // 目标卷无旁路 meta 更新能力（如裸本地卷/无 meta 桶的外部卷）
	}
	var etag, checksum, authority string
	if env.result != nil {
		etag, checksum, authority = env.result.ETag, env.result.Checksum, env.result.AuthorityHash
	}
	extra := buildTransferSourceMeta(env.task, etag, checksum, authority)
	if len(extra) == 0 {
		return
	}
	if err := ue.UpdateMetaExtra(env.ctx, env.rel, extra); err != nil {
		m.logger.Warn("转存来源 meta 写入失败（不阻断转存）",
			"task_id", env.task.ID, "volume", env.task.Transfer.Volume, "rel", env.rel, "error", err)
	}
}
