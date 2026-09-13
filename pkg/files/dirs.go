// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/pathguard"
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
	dirname := r.URL.Query().Get("dirname")
	if dirname == "" {
		s.sendJSON(w, UploadResponse{Success: false, Message: "dirname 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的目录名: " + err.Error()}, http.StatusBadRequest)
		return
	}
	// 路径映射与卷无关（user/<path> 相对各卷租户根），用默认租户做纯路径校验；
	// 实际落盘目录选 owner 视图内首个卷（默认卷优先——默认卷开放时即默认租户，零回归；
	// 默认卷被 ACL 排除时落到视图卷，绝不经默认租户直写默认卷遗留，AD-6 闭合）。
	owner := normalizeOwner(s.rt.actorOf(r))
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	rel, ok := tnt0.UserRel(remotePath)
	if !ok {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	target := s.primaryViewTenant(owner)
	if target == nil || target.Root() == nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}

	if err := target.Root().MkdirAll(rel, 0755); err != nil {
		s.rt.logger().Error(errMsgCreateDirFailed, "dir", remotePath, "error", err)
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgCreateDirFailed}, http.StatusInternalServerError)
		return
	}

	s.rt.logger().Info("目录已创建", "dir", remotePath)
	s.sendJSON(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已创建: %s", remotePath)}, http.StatusOK)
}

// Rmdir 删除指定目录（含所有内容）。?dirname=path&force=true
// 已迁移到 Tenant API：路径映射到 user 桶（<root>/<owner>/user/<rel>）。
// 多卷（任务 5）：同一子目录可能因换卷在多个卷上都存在（main/disk2 各有其文件），故按 owner
// 视图逐卷定位、对**每个存在该目录的卷**删除其子树（默认卷优先定位；目录非文件，不受 AD-4
// 文件唯一性约束，可跨卷并存）。递归删除用 root.RemoveAll（os.Root 保证符号链接不逃逸）；
// checksum 从 per-tenant store 清理 rel 前缀与 rel 自身。
func (s *Service) Rmdir(w http.ResponseWriter, r *http.Request) {
	dirname := r.URL.Query().Get("dirname")
	if dirname == "" {
		s.sendJSON(w, UploadResponse{Success: false, Message: "dirname 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的目录名: " + err.Error()}, http.StatusBadRequest)
		return
	}
	// 归一 owner（空 → anonymous）：目录探测/删除与列表/写路径同键，未认证请求归属 anonymous。
	owner := normalizeOwner(s.rt.actorOf(r))
	// 路径映射与卷无关，用默认租户做纯路径校验；卷感知只决定目录落到哪些卷的租户根。
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	rel, ok := tnt0.UserRel(remotePath)
	if !ok {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}

	// 收集 owner 视图内存在该目录的卷租户（默认卷优先；目录不存在于任何卷 → 404）。
	type rmTarget struct {
		volName string
		tnt     *storage.Tenant
	}
	var targets []rmTarget
	if s.rt.volSet() == nil {
		targets = append(targets, rmTarget{volName: "", tnt: tnt0})
	} else {
		view := volume.AllowedVolumes(s.rt.volSet().All(), owner)
		for _, v := range view {
			rt := s.rt.volSet().Root(v.Name)
			if rt == nil {
				continue
			}
			// 探测用卷根相对 <owner>/<rel>（不创建租户目录）；确认存在再取租户操作。
			if _, err := rt.Stat(owner + "/" + rel); err != nil {
				continue
			}
			tnt := s.rt.volumeTenant(v.Name, owner)
			if tnt == nil || tnt.Root() == nil {
				continue
			}
			targets = append(targets, rmTarget{volName: v.Name, tnt: tnt})
		}
	}
	if len(targets) == 0 {
		s.sendJSON(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
		return
	}

	// 符号链接 / 非目录检查与 TOCTOU 二次检查：在首个命中卷（默认卷优先）执行，错误语义与
	// 单卷一致（目录不存在 404 / 符号链接或非目录 400）。
	primary := targets[0]
	if err := validateRmdirTarget(primary.tnt.Root(), rel); err != nil {
		switch {
		case os.IsNotExist(err):
			s.sendJSON(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
		case errors.Is(err, errRmdirSymlink):
			s.sendJSON(w, UploadResponse{Success: false, Message: "不允许删除符号链接"}, http.StatusBadRequest)
		case errors.Is(err, errRmdirNotDir):
			s.sendJSON(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
		default:
			s.sendJSON(w, UploadResponse{Success: false, Message: "访问目录失败"}, http.StatusInternalServerError)
		}
		return
	}

	// force 必须为 true 才执行删除（避免误删）
	force := r.URL.Query().Get("force") == "true"
	if !force {
		s.sendJSON(w, UploadResponse{Success: false, Message: "请使用 ?force=true 确认删除"}, http.StatusBadRequest)
		return
	}

	// 逐卷删除存在该目录的子树；每卷删除前收集树内文件 {rel,size}（per-file 分键释放 owner
	// 全局 Scope——跨卷合计语义正确，文件 rel 唯一故不双计），并按所在卷释放卷容量池。
	var allFiles []rmdirFileStat
	for _, tg := range targets {
		root := tg.tnt.Root()
		var dirFiles []rmdirFileStat
		sumRootDirFiles(root, rel, &dirFiles)
		if err := root.RemoveAll(rel); err != nil {
			s.rt.logger().Error("删除目录失败", "dir", remotePath, "error", err)
			s.sendJSON(w, UploadResponse{Success: false, Message: "删除目录失败"}, http.StatusInternalServerError)
			return
		}
		allFiles = append(allFiles, dirFiles...)
		// 卷容量池释放：本卷被删字节 = dirFiles 之和。
		if tg.volName != "" && s.rt.volSet() != nil {
			if pool := s.rt.volSet().Pool(tg.volName); pool != nil {
				var volBytes int64
				for _, f := range dirFiles {
					volBytes += f.size
				}
				if volBytes > 0 {
					pool.ReleaseCommitted(volBytes)
				}
			}
		}
	}

	// 删除成功后按各文件实际子 Scope（按 rel 解析）释放配额占用。
	for _, f := range allFiles {
		if scope := s.rt.quotaScope(owner, f.rel); scope != nil {
			scope.ReleaseUsage(f.size)
		}
	}

	// 清理 per-tenant checksum store 中该目录下所有文件的记录（key = rel，无 owner 前缀）。
	// 使用 "/" 分隔符，与 ChecksumStore 的 key 格式约定保持一致（所有 key 使用 filepath.ToSlash 格式）。
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.DeletePrefix(rel + "/")
		// 清理目录自身的 checksum 记录（如果存在）
		cs.Delete(rel)
	}

	s.rt.logger().Info("目录已删除", "dir", remotePath)
	s.sendJSON(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已删除: %s", remotePath)}, http.StatusOK)
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
