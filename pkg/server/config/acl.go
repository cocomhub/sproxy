// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// acl.go：把配置层 VolumeACLConfig 映射为 pkg/volume.ACL 纯域类型（配置 → 领域）。
package config

import (
	"log/slog"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// ParseVolumeACL 把配置层 VolumeACLConfig 解析为 pkg/volume.ACL 纯域类型。
// 未配/缺省（nil）→ deny + 空名单 = 默认开放（AD-6 有意语义，兼容默认卷/旧单根）。
// 空 mode（防御，上游 SetDefaults 已归一 deny）→ deny。owners 逐项转 map（供 Authorize O(1)）。
//
// log 用于记录被丢弃的 mesh_readers 条目（禁止静默失败）；nil 时经 slogutil.Default 回落
// slog.Default()（共享实现，见 internal/slogutil），调用方无需保证非 nil。
func ParseVolumeACL(ac *VolumeACLConfig, log *slog.Logger) volume.ACL {
	log = slogutil.Default(log)
	acl := volume.ACL{Mode: volume.ModeDeny, Owners: map[string]struct{}{}}
	if ac == nil {
		return acl
	}
	acl.Mode = volume.Mode(ac.Mode)
	if acl.Mode == "" {
		acl.Mode = volume.ModeDeny
	}
	for _, o := range ac.Owners {
		acl.Owners[o] = struct{}{}
	}
	// Y 一期：跨节点只读授权条目（指纹归一为规范形；非法值已由 Config.Validate 拒绝）。
	for _, mr := range ac.MeshReaders {
		// 解析失败即丢弃该条目（fail-closed 纵深防御）：降级保留一个语义不明的
		// 字符串会在未来改动中被误用。生产路径上非法指纹已由 Config.Validate 响亮
		// 拒绝，本分支不可达；此处仅作装配层兜底。
		norm, err := tunnel.ParseFingerprint(mr.Fingerprint)
		if err != nil {
			// 生产路径上此处不可达（Config.Validate 已响亮拒绝畸形指纹）；留下告警是
			// 「禁止静默失败」原则的落实——安全相关的 ACL 条目被丢弃必须有痕迹。
			log.Warn("丢弃 mesh_readers 条目：指纹非法", "node", mr.Node, "owner", mr.Owner, "error", err)
			continue
		}
		// Y 二期 P3：scope 透传（未知值丢弃条目——与畸形指纹同策略：降级保留一个语义
		// 不明的权限范围，可能在未来改动中被当成「有权限」使用，故宁可丢掉并留痕）。
		scope, ok := volume.NormalizeMeshScope(mr.Scope)
		if !ok {
			log.Warn("丢弃 mesh_readers 条目：scope 非法", "node", mr.Node, "owner", mr.Owner, "scope", mr.Scope)
			continue
		}
		acl.MeshReaders = append(acl.MeshReaders, volume.MeshReader{
			Node: mr.Node, Fingerprint: norm, Owner: mr.Owner, Scope: scope,
		})
	}
	return acl
}
