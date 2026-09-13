// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"fmt"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// errMsgCreateDirFailed 是 mkdir 失败响应文案（随本族从 pkg/server/errors.go 迁入；
// 该常量在 pkg/server 已无其他使用者）。
const errMsgCreateDirFailed = "创建目录失败"

// sumRootDirSize 递归统计租户根内 rel 子树下所有普通文件的总字节数（rmdir 配额释放用）。
// 经 root.ReadDir 相对遍历（os.Root 防符号链接逃逸），跳过符号链接（Lstat 语义不计数），
// 子目录递归累加。rmdir 删除前调用，删除成功后按该字节数 ReleaseUsage（I2 修复）。
// rmdirFileStat 是 rmdir 配额释放收集项：每个被删文件的完整相对路径（含功能桶前缀）
// 与其字节数。按文件 rel 分键释放到对应子 Scope（bucket_limits 子目录配额一致）。
type rmdirFileStat struct {
	rel  string
	size int64
}

// sumRootDirFiles 递归收集租户根内 rel 子树下所有普通文件（符号链接跳过）的 {rel, size}，
// rmdir 配额释放用（per-file 分键，保证删除释放落到正确的子目录 Scope）。
func sumRootDirFiles(root *storage.Root, rel string, out *[]rmdirFileStat) {
	entries, err := root.ReadDir(rel)
	if err != nil {
		return
	}
	for _, e := range entries {
		childRel := rel + "/" + e.Name()
		if e.IsDir() {
			sumRootDirFiles(root, childRel, out)
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if info, err := e.Info(); err == nil {
			*out = append(*out, rmdirFileStat{rel: childRel, size: info.Size()})
		}
	}
}

// primaryViewTenant 返回 owner 视图内首个卷的租户（写新目录/新文件等「不跨卷写」入口用）。
// 默认卷在视图时即默认租户（声明序首卷，单卷零回归）；默认卷被 ACL 排除时落到首个其它视图卷。
// 视图全空 / 卷租户不可用返回 nil（调用方按 400 fail-closed）。VolSet nil（旧装配）回落默认租户。
//
// 本函数自 pkg/server/volumes.go **原样下沉**（函数体逐字未改，仅接缝项 h.X → s.rt.X()）：
// 它只用 volume.AllowedVolumes（pkg/volume，G0 基础包，领域包可直接 import）与接缝已有的
// VolSet/VolumeTenant/TenantFor，不含 Handlers 私有状态——属**领域内纯策略**，
// 不必占接缝字段（接缝只放「必须由装配层注入」的项）。
func (s *Service) primaryViewTenant(owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	if s.rt.volSet() == nil {
		return s.rt.tenantOf(owner)
	}
	for _, v := range volume.AllowedVolumes(s.rt.volSet().All(), owner) {
		tnt := s.rt.volumeTenant(v.Name, owner)
		if tnt != nil && tnt.Root() != nil {
			return tnt
		}
	}
	return nil
}

// Mkdir 创建指定子目录。?dirname=path
// 已迁移到 Tenant API：用户目录映射到 user 桶内（<root>/<owner>/user/<rel>），
// UserRel 逐段段名校验（拒绝 .__ 内部前缀、功能桶引用、保留设备名等），
// 无需再单独内部目录守卫。
func (s *Service) Mkdir(w http.ResponseWriter, r *http.Request) {
	res, err := s.MakeDir(s.rt.actorOf(r), r.URL.Query().Get("dirname"))
	if err != nil {
		he := asHTTPError(err)
		s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
		return
	}
	s.sendJSON(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已创建: %s", res.RemotePath)}, http.StatusOK)
}

// Rmdir 删除指定目录（含所有内容）。?dirname=path&force=true
// 已迁移到 Tenant API：路径映射到 user 桶（<root>/<owner>/user/<rel>）。
// 多卷（任务 5）：同一子目录可能因换卷在多个卷上都存在（main/disk2 各有其文件），故按 owner
// 视图逐卷定位、对**每个存在该目录的卷**删除其子树（默认卷优先定位；目录非文件，不受 AD-4
// 文件唯一性约束，可跨卷并存）。递归删除用 root.RemoveAll（os.Root 保证符号链接不逃逸）；
// checksum 从 per-tenant store 清理 rel 前缀与 rel 自身。
func (s *Service) Rmdir(w http.ResponseWriter, r *http.Request) {
	res, err := s.RemoveDir(s.rt.actorOf(r), r.URL.Query().Get("dirname"), r.URL.Query().Get("force") == "true")
	if err != nil {
		he := asHTTPError(err)
		s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
		return
	}
	s.sendJSON(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已删除: %s", res.RemotePath)}, http.StatusOK)
}

// errRmdirSymlink / errRmdirNotDir 是 rmdir 目标校验的哨兵错误（供状态映射）。
var (
	errRmdirSymlink = fmt.Errorf("rmdir: 不允许删除符号链接")
	errRmdirNotDir  = fmt.Errorf("rmdir: 指定路径不是目录")
)

// validateRmdirTarget 对 rel 做 Lstat 校验 + TOCTOU 二次校验：不存在返回 fs.ErrNotExist；
// 符号链接返回 errRmdirSymlink；非目录返回 errRmdirNotDir。其余错误原样返回。
func validateRmdirTarget(root *storage.Root, rel string) error {
	for range 2 { // 两次 Lstat（TOCTOU 防御，与旧实现一致）
		stat, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			return errRmdirSymlink
		}
		if !stat.IsDir() {
			return errRmdirNotDir
		}
	}
	return nil
}
