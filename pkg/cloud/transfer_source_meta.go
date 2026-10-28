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
//	source            = "transfer"（固定；与 upload 来源区分）
//	source_url        = task.URL（源 URL，含 spx_target 等定位参数）
//	etag              = result.ETag（服务端唯一性标识；空则不写）
//	checksum          = result.Checksum（本地落盘文件 SHA-256 十六进制，小写；空则不写）
//	checksum_algorithm = "sha256"（checksum 的算法标识——通用字段自描述，便于校验维护）
//	authority_hash    = result.AuthorityHash（服务端带外权威 hash；PikPak 场景即 GCID）
//	authority_hash_algorithm = "pikpak-gcid"（PikPak GCID = sha1(concat(sha1(分块)))；
//	                         其他下载器/场景按各自权威算法标识，空则不写）
//	transferred_at    = RFC3339 转存时刻
//	<客户端 meta_extra 各键>（同名键：客户端值优先——客户端更懂业务语义）
package cloud

import (
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// transferSourceMetaKeys 是服务端写入的来源键（客户端 meta_extra 同名键**不覆盖**
// 服务端来源语义，除非显式提供——见 buildTransferSourceMeta 的合并次序）。
const (
	metaKeySource        = "source"
	metaKeySourceURL     = "source_url"
	metaKeyETag          = "etag"
	metaKeyChecksum      = "checksum"
	metaKeyChecksumAlg   = "checksum_algorithm" // 算法标识（sha256）——通用 checksum 字段自描述
	metaKeyAuthorityHash = "authority_hash"
	metaKeyAuthorityAlg  = "authority_hash_algorithm" // 算法标识（pikpak-gcid 等）——通用字段自描述
	metaKeyTransferredAt = "transferred_at"
)

// buildTransferSourceMeta 组装转存来源 meta（服务端来源键 + 客户端 meta_extra）。
// 次序：先服务端来源，再客户端 extra（客户端同名键覆盖服务端——业务语义优先）；
// 空值不写入（不产生 etag:"" 噪声）。
// resultHashAlgs 从下载结果取算法标识（下载方声明；空则不写标识字段）。
func resultHashAlgs(result *downloader.Result) (checksumAlg, authorityAlg string) {
	if result == nil {
		return "", ""
	}
	return result.ChecksumAlgorithm, result.AuthorityHashAlgorithm
}

func buildTransferSourceMeta(task *CloudTask, etag, checksum, authorityHash, checksumAlg, authorityAlg string) map[string]any {
	meta := map[string]any{
		metaKeySource:        "transfer",
		metaKeyTransferredAt: time.Now().UTC().Format(time.RFC3339),
	}
	applyIf(meta, task != nil && task.URL != "", metaKeySourceURL, task.URL)
	applyIf(meta, etag != "", metaKeyETag, etag)
	applyIf(meta, checksum != "", metaKeyChecksum, checksum)
	applyIf(meta, checksum != "" && checksumAlg != "", metaKeyChecksumAlg, checksumAlg)
	applyIf(meta, authorityHash != "", metaKeyAuthorityHash, authorityHash)
	applyIf(meta, authorityHash != "" && authorityAlg != "", metaKeyAuthorityAlg, authorityAlg)
	if task != nil {
		for k, v := range task.MetaExtra {
			if k != "" && v != nil {
				meta[k] = v // 客户端值覆盖同名服务端键（业务语义优先）
			}
		}
	}
	return meta
}

// applyIf cond 为真时写入键值（避免嵌套 if 抬高复杂度）。
func applyIf(meta map[string]any, cond bool, k string, v any) {
	if cond {
		meta[k] = v
	}
}

// recordTransferSourceMeta 把来源 meta 写入目标卷（经 UpdateMetaExtra 旁路合并）。
// 无能力（卷未实现 meta extra 更新）→ 跳过；写失败 → Warn（不阻断转存）。
func (m *CloudDownloadManager) recordTransferSourceMeta(env *transferEnv) {
	ue, ok := env.targetFS.(metaExtraUpdater)
	if !ok {
		return // 目标卷无旁路 meta 更新能力（如裸本地卷/无 meta 桶的外部卷）
	}
	var etag, checksum, authority, checksumAlg, authorityAlg string
	if env.result != nil {
		etag, checksum, authority = env.result.ETag, env.result.Checksum, env.result.AuthorityHash
		checksumAlg, authorityAlg = resultHashAlgs(env.result)
	}
	extra := buildTransferSourceMeta(env.task, etag, checksum, authority, checksumAlg, authorityAlg)
	if len(extra) == 0 {
		return
	}
	if err := ue.UpdateMetaExtra(env.ctx, env.rel, extra); err != nil {
		m.logger.Warn("转存来源 meta 写入失败（不阻断转存）",
			"task_id", env.task.ID, "volume", env.task.Transfer.Volume, "rel", env.rel, "error", err)
	}
}
