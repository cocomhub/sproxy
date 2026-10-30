// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// fingerprints.go：从配置派生 mesh 对端指纹集合（只读面 / 写面 pin 列表）。
//
// 这些是**纯配置派生**（输入只有 *Config），运行时 listener 只消费结果；
// 放在 config 包内保证「配置 → pin 列表」的判定与校验同源，不散落在监听器文件里。
package config

import (
	"strings"

	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// MeshReaderFingerprints 返回所有卷 mesh_readers 的指纹（归一化后去重）。
//
// 归一化走 tunnel.ParseFingerprint（权威规范形）；解析失败的条目退回「去空白 + 小写」
// 原样保留——非法指纹已由 Config.Validate 响亮拒绝，此处不静默丢弃（丢弃会让
// 「配置了一条却未生效」变成静默失败）。
func MeshReaderFingerprints(cfg *Config) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, v := range cfg.Volumes {
		if v.ACL == nil {
			continue
		}
		for _, mr := range v.ACL.MeshReaders {
			fp := strings.ToLower(strings.TrimSpace(mr.Fingerprint))
			if norm, err := tunnel.ParseFingerprint(mr.Fingerprint); err == nil {
				fp = norm
			}
			if fp == "" {
				continue
			}
			if _, dup := seen[fp]; dup {
				continue
			}
			seen[fp] = struct{}{}
			out = append(out, fp)
		}
	}
	return out
}

// ReadListenerPins 返回 remote_read listener 信任的对端指纹集合 = mesh_readers 指纹 ∪
// 集群出口凭证 Recipient（评审 M3：纯凭证部署——无 mesh_readers 但配置了凭证池——应
// 可启动；凭证 Recipient 是签发方授权的出口指纹，与 mesh_readers 同为「接受的对端
// 白名单」，纳入双向 pin 保持「无 pin 就不接受任意对端」安全论证不变）。
func ReadListenerPins(cfg *Config) []string {
	out := MeshReaderFingerprints(cfg)
	seen := make(map[string]struct{}, len(out))
	for _, p := range out {
		seen[p] = struct{}{}
	}
	for _, ec := range cfg.Cluster.Credentials {
		if c, perr := clustercred.ParseCredential(ec.Encoded); perr == nil && c.Recipient != "" {
			fp := strings.ToLower(strings.TrimSpace(c.Recipient))
			if _, dup := seen[fp]; dup {
				continue
			}
			seen[fp] = struct{}{}
			out = append(out, fp)
		}
	}
	return out
}

// MeshWriterFingerprints 返回所有**scope 授予写**（write|rw）的条目指纹（归一化后去重）。
//
// 与 meshReaderFingerprints（只读面 pin 列表，取全部条目）的差别是本函数只收能写的指纹：
// 只读对端因此**连写面握手都建立不了**——这是「写通路物理上不存在」的直接落实，也与
// 只读面的 pin 策略各管一边、互不放大权限。
//
// 归一化走 tunnel.ParseFingerprint（权威规范形）；解析失败的条目退回「去空白 + 小写」
// 原样保留——非法指纹已由 Config.Validate 响亮拒绝，此处不静默丢弃。scope 未知/未授予写
// 的条目**跳过**（判定单源在 pkg/volume.NormalizeMeshScope）。
func MeshWriterFingerprints(cfg *Config) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, v := range cfg.Volumes {
		if v.ACL == nil {
			continue
		}
		for _, mr := range v.ACL.MeshReaders {
			if !scopeGrantsWrite(mr.Scope) {
				continue
			}
			out = appendUniqueFingerprint(seen, out, mr.Fingerprint)
		}
	}
	return out
}

// appendUniqueFingerprint 归一化一条指纹并入去重集合；空/重复时原样返回 out。
func appendUniqueFingerprint(seen map[string]struct{}, out []string, raw string) []string {
	fp := strings.ToLower(strings.TrimSpace(raw))
	if norm, err := tunnel.ParseFingerprint(raw); err == nil {
		fp = norm
	}
	if fp == "" {
		return out
	}
	if _, dup := seen[fp]; dup {
		return out
	}
	seen[fp] = struct{}{}
	return append(out, fp)
}

// scopeGrantsWrite 判定配置里的 scope 是否授予写（write / rw）；空值与未知值均不授予
// （写面 pin 宁可少收——漏收的后果是「对端连不上」而非「越权可写」）。
func scopeGrantsWrite(scope string) bool {
	s, ok := volume.NormalizeMeshScope(scope)
	if !ok {
		return false
	}
	return s == volume.MeshScopeWrite || s == volume.MeshScopeRW
}
