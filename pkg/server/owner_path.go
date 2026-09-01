// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"path/filepath"
	"strings"
)

// ownerFromRequest 返回请求 ctx 中的操作主体（未认证返回 ""）。
func ownerFromRequest(r *http.Request) string {
	return ActorFrom(r.Context())
}

// ownerUploadsDirFor 返回指定 owner 的存储根目录。
// 多租户隔离：owner 非空时用户文件存 uploadsDir/<owner>/ 子目录；未认证（owner 空）
// 直接使用 uploadsDir（单租户兼容，避免引入默认目录语义）。
// 审查 I2：owner 作为路径段前必须校验（fail-closed）——非法 owner（如 ..、.__cloud__、
// 含 / 或 \、Windows 设备名）会让 owner 根逃出 uploadsDir 或与内部目录重合，破坏隔离。
func (h *Handlers) ownerUploadsDirFor(owner string) string {
	cfg := h.cfgPtr.Load()
	if cfg == nil {
		return ""
	}
	if owner == "" {
		return cfg.UploadsDir
	}
	if !validOwnerDirName(owner) {
		// 非法 owner 回落单租户根（不 panic）；调用方（认证层）应保证 owner 合法，
		// 此处纵深防御——避免逃逸/内部目录重合。
		return cfg.UploadsDir
	}
	return filepath.Join(cfg.UploadsDir, owner)
}

// validOwnerDirName 校验 owner 字符串是否可安全作为 uploadsDir 下的单路径段。
// 拒绝：空、. / ..、含路径分隔符（/ \）、以 .__ 开头（服务端内部目录约定）、
// Windows 非法字符与保留设备名（CON/NUL/PRN/AUX/COM1-9/LPT1-9）。
func validOwnerDirName(owner string) bool {
	if owner == "" || owner == "." || owner == ".." {
		return false
	}
	if strings.ContainsAny(owner, `/\`) {
		return false
	}
	if strings.HasPrefix(owner, ".__") {
		return false
	}
	if strings.ContainsAny(owner, `<>:"|?*`) {
		return false
	}
	// Windows 保留设备名（大小写不敏感，对齐 cloudfilename.Safe）。
	upper := strings.ToUpper(owner)
	if upper == "CON" || upper == "NUL" || upper == "PRN" || upper == "AUX" {
		return false
	}
	if strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT") {
		if len(upper) > 3 {
			if upper[3] >= '1' && upper[3] <= '9' {
				return false
			}
		}
	}
	return true
}

// safePathForOwner 在指定 owner 的存储根下安全拼接 remotePath（越界返回空串）。
func (h *Handlers) safePathForOwner(owner, remotePath string) string {
	if remotePath == "" {
		return ""
	}
	return joinSafePath(h.ownerUploadsDirFor(owner), remotePath)
}

// safePathFor 在请求者 owner 的存储根下安全拼接 remotePath（越界返回空串）。
func (h *Handlers) safePathFor(r *http.Request, remotePath string) string {
	return h.safePathForOwner(ownerFromRequest(r), remotePath)
}

// resolveAndValidateFileForOwner 校验文件名并返回指定 owner 租户 user 桶下的相对路径
// （如 user/dir/f.txt）。供批量操作（ctx 无 *http.Request）使用；校验失败返回 ("", "", false)。
func (h *Handlers) resolveAndValidateFileForOwner(owner, filename string) (remotePath, rel string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		return "", "", false
	}
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return "", "", false
	}
	rel, ok = tnt.UserRel(remotePath)
	if !ok {
		return "", "", false
	}
	return remotePath, rel, true
}

// checksumStoreKey owner 非空时前缀 owner，避免跨租户同路径 checksum 冲突。
// 未认证（owner 空）沿用原 key（单租户兼容）。
func checksumStoreKey(owner, remotePath string) string {
	if owner == "" {
		return remotePath
	}
	return owner + "/" + remotePath
}

// checksumKeyFor 生成 ChecksumStore 的 owner 作用域 key。
func (h *Handlers) checksumKeyFor(r *http.Request, remotePath string) string {
	return checksumStoreKey(ownerFromRequest(r), remotePath)
}

// cloudArchiveOwnerDir 返回 owner 的归档子目录名（未认证返回空串，直接用归档根目录）。
func cloudArchiveOwnerDir(owner string) string {
	if owner == "" {
		return ""
	}
	return owner
}

// cloudArchivePathFor 在 uploadsDir/.__cloud_archives__/[owner/] 下安全拼接归档名。
// 按 owner 隔离：认证用户归档存各自子目录，未认证（owner 空）存归档根目录。
// 返回空串表示配置缺失或路径越界（防穿越纵深防御）。
func (h *Handlers) cloudArchivePathFor(r *http.Request, name string) string {
	cfg := h.cfgPtr.Load()
	if cfg == nil {
		return ""
	}
	base := filepath.Join(cfg.UploadsDir, cloudArchiveDirName)
	if ownerDir := cloudArchiveOwnerDir(ownerFromRequest(r)); ownerDir != "" {
		base = filepath.Join(base, ownerDir)
	}
	fullPath := filepath.Join(base, name)
	if !IsPathWithin(fullPath, base) {
		return ""
	}
	return fullPath
}

// hasInternalDirAtAnyDepth 检查 rel 路径中任意层级是否包含指定内部目录名。
// 多租户 owner 隔离后，版本目录等内部目录可能出现在 owner 子目录下
// （uploadsDir/<owner>/.__versions__/<path>），StorageManager/stats 扫描分类需识别。
func hasInternalDirAtAnyDepth(rel, dirName string) bool {
	if rel == dirName || strings.HasPrefix(rel, dirName+"/") {
		return true
	}
	return strings.Contains(rel, "/"+dirName+"/") || strings.HasSuffix(rel, "/"+dirName)
}
