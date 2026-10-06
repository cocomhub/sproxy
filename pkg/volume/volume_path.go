// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

// volume_path.go 提供卷内路径统一工具（2026-10-05 用户裁定）：
//
// `Volume.ResolveOwnerPath(owner, bucket, rel)` 是「owner 相对路径 → 卷内最终键」的
// **唯一入口**——上传 / 下载 / 转存三侧统一经它，使用方不关心卷内键算法：
//
//   - **权限**：owner 无权访问卷 → ErrVolumeNotAuthorized（fail-closed，立即报错，
//     不暴露卷内布局/存在性）；
//   - **路径安全**：归一 + 逐段校验（拒绝 ../ 逃逸、绝对路径、空段、非法段名 /
//     Windows 保留名 / `.__` 内部前缀——防注入）；
//   - **键空间自动适配**：共享卷（多用户）→ `<owner>/<bucket>/<rel>`（owner 前缀
//     隔离，不同用户靠机制隔离互不感知）；独享卷（用户外部网盘）→ `<bucket>/<rel>`
//     （无前缀直接路径存取）。
//
// bucket 是**桶名**（调用方必须显式传入，2026-10-05 用户裁定）："user"（用户文件）、
// "meta"（元数据）、"cloud"（云任务产物）、"archive"（归档）等——与 storage 既有
// featureBuckets 语义对齐，不假设只有 user 桶。rel 为桶内相对路径（可为空 =
// 仅桶自身，如 mkdir user）。
//
// 与 storage.NormalizeRemote/ValidSegmentName 复用同一路径语义（单一权威），
// volume 包 import storage 无环（storage 不依赖 volume）。

import (
	"errors"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// ErrVolumeNotAuthorized 是用户无权访问卷的哨兵错误（fail-closed：立即报错，
// 不静默降级到其它卷/默认路径——防 ACL 绕过）。
var ErrVolumeNotAuthorized = errors.New("volume: 用户无权访问该卷")

// ErrInvalidUserPath 是用户路径非法（逃逸/注入/保留名）的哨兵错误。
var ErrInvalidUserPath = errors.New("volume: 非法用户路径")

// ResolveOwnerPath 把 owner 相对路径映射为卷内最终键（见文件头注释）。
// bucket 为桶名（"user"/"meta"/"cloud"/...）；rel 为桶内相对路径（可空）。
// 任意参数含绝对路径 / ../ 逃逸 / 空段 / 非法段 → ErrInvalidUserPath。
func (v Volume) ResolveOwnerPath(owner, bucket, rel string) (string, error) {
	// 1. 权限门（fail-closed）：无权访问本卷立即报错，不进入路径计算。
	if !v.Authorize(owner) {
		return "", ErrVolumeNotAuthorized
	}
	// 2. bucket 校验（单段合法名——防桶名注入/多段拼写）。
	if !storage.ValidSegmentName(bucket) {
		return "", ErrInvalidUserPath
	}
	// 3. rel 归一（复用 storage 单一权威：拒绝对路径/../空段/Windows 卷名）。
	//    空 rel（仅桶）单独放行（mkdir user 等场景）。
	norm := ""
	if rel != "" {
		n, ok := storage.NormalizeRemote(rel)
		if !ok {
			return "", ErrInvalidUserPath
		}
		norm = n
		// 逐段校验（ValidSegmentName：拒绝对端名/保留设备名/.__ 内部前缀）。
		for seg := range strings.SplitSeq(norm, "/") {
			if !storage.ValidSegmentName(seg) {
				return "", ErrInvalidUserPath
			}
		}
	}
	// 4. 键空间自动适配：共享卷加 owner 前缀隔离；独享卷无前缀直接存取。
	base := bucket
	if norm != "" {
		base = bucket + "/" + norm
	}
	if v.Shared() {
		return storage.NormalizeOwner(owner) + "/" + base, nil
	}
	return base, nil
}

// ResolveUserPath 是 ResolveOwnerPath 的 user 桶便捷封装（用户文件路径，
// 2026-10-05）：等价 v.ResolveOwnerPath(owner, "user", rel)。
func (v Volume) ResolveUserPath(owner, userPath string) (string, error) {
	return v.ResolveOwnerPath(owner, "user", userPath)
}

// UserVisibleRel 把卷键空间路径归一为用户可见相对路径（占用写保护/子目录坐标比较用）：
//   - 独享卷键 `user/<rel>` → `<rel>`；
//   - 共享卷键 `<owner>/user/<rel>` → `<rel>`（跳过 owner 段）；
//   - 无法识别格式（非 user 桶前缀形态）→ 原样返回（占用比较仅字符串匹配，不误伤正常路径）。
//
// 与 files 域 `user/<rel>`（租户根相对）归一结果一致（两者最终都指向卷 user 桶内用户可见
// 坐标）。pkg/cloud 转存与 pkg/server 转存预检共用本入口，避免复制键算法。
func UserVisibleRel(key string) string {
	if key == "" {
		return ""
	}
	key = strings.TrimPrefix(key, "/")
	if after, ok := strings.CutPrefix(key, "user/"); ok {
		return after
	}
	if _, after, ok := strings.Cut(key, "/"); ok {
		if after2, ok2 := strings.CutPrefix(after, "user/"); ok2 {
			return after2
		}
	}
	return key
}
