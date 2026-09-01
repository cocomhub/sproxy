// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
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
// 保留供测试与后续清理引用（P5 删除旧布局实现时一并移除）。
func (h *Handlers) safePathForOwner(owner, remotePath string) string {
	if remotePath == "" {
		return ""
	}
	return joinSafePath(h.ownerUploadsDirFor(owner), remotePath)
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

// cloudArchivePathFor 解析 kind=cloud_archive 归档在请求者租户 archive 桶下的相对路径。
// 返回 (tenant, rel)（rel 形如 "archive/<name>"）；租户不可用或名称未通过 FeatureRel
// 校验（单文件名已由 validateCloudArchiveName 前置校验，此处纵深防御）时返回 (nil, "")。
func (h *Handlers) cloudArchivePathFor(r *http.Request, name string) (*storage.Tenant, string) {
	tnt := h.tenantFor(ownerFromRequest(r))
	if tnt == nil {
		return nil, ""
	}
	rel, ok := tnt.FeatureRel("archive", name)
	if !ok {
		return nil, ""
	}
	return tnt, rel
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
