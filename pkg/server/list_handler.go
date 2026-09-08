// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// fileInfo 是文件列表中的条目结构。
type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"` // UnixNano
	IsDir    bool   `json:"is_dir"`   // 是否为目录
	// Volume 是条目所在卷名（多卷聚合列表新增字段；向后兼容——旧客户端忽略未知键。
	// 目录条目为聚合视图下的逻辑目录（可跨卷并存），不绑定单卷，Volume 为空）。
	Volume string `json:"volume"`
}

// listResponse 是文件列表的响应结构。
type listResponse struct {
	Files  []fileInfo `json:"files"`
	Total  int        `json:"total"`
	Offset int        `json:"offset"`
	Limit  int        `json:"limit"`
}

// parsePagination 从请求查询参数中解析 offset 和 limit。
// offset 默认 0，limit 默认 1000（上限 10000）。
func parsePagination(r *http.Request) (offset, limit int) {
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil {
			offset = n
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	return
}

// resolveListDir 处理 listFiles 的 subdir 参数，返回请求者租户 user 桶内的目标目录
// 绝对路径（供 os.ReadDir）。默认根 = user 桶绝对路径；subdir 经 UserRel 映射到
// user/<subdir>（功能桶名首段作为用户路径合法，解析到 user 桶内，功能桶天然不可枚举）。
// ValidateFilePath 格式校验保留（防穿越/非法字符）。
func (h *Handlers) resolveListDir(w http.ResponseWriter, r *http.Request) (targetDir string, ok bool) {
	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		h.logger.Warn("租户不可用", "owner", ownerFromRequest(r))
		sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
		return "", false
	}
	root := tnt.Root()
	targetDir, ok = root.Abs(tnt.UserRoot())
	if !ok {
		h.logger.Warn("派生 user 桶路径失败", "owner", ownerFromRequest(r))
		sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
		return "", false
	}
	if subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/"); subdir != "" {
		if _, err := ValidateFilePath(subdir); err != nil {
			h.logger.Warn("无效的子目录", "subdir", subdir, "error", err.Error())
			sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
			return "", false
		}
		rel, uok := tnt.UserRel(subdir)
		if !uok {
			h.logger.Warn("无效的子目录路径", "subdir", subdir)
			sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
			return "", false
		}
		targetDir, ok = root.Abs(rel)
		if !ok {
			h.logger.Warn("子目录路径越界", "subdir", subdir)
			sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
			return "", false
		}
	}
	return targetDir, true
}

// sortFileEntries 按指定字段和顺序排序文件条目。
func sortFileEntries(entries []fileInfo, sortBy, sortOrder string) {
	if sortOrder != "desc" {
		sortOrder = "asc"
	}
	switch sortBy {
	case "size":
		sort.SliceStable(entries, func(i, j int) bool {
			if sortOrder == "desc" {
				return entries[i].Size > entries[j].Size
			}
			return entries[i].Size < entries[j].Size
		})
	case "time":
		sort.SliceStable(entries, func(i, j int) bool {
			if sortOrder == "desc" {
				return entries[i].ModTime > entries[j].ModTime
			}
			return entries[i].ModTime < entries[j].ModTime
		})
	default: // "name"
		if sortOrder == "desc" {
			sort.SliceStable(entries, func(i, j int) bool {
				return entries[i].Name > entries[j].Name
			})
		} else {
			sort.SliceStable(entries, func(i, j int) bool {
				return entries[i].Name < entries[j].Name
			})
		}
	}
}

// paginateEntries 对文件列表进行分页。
func paginateEntries(entries []fileInfo, offset, limit int) []fileInfo {
	total := len(entries)
	start := min(offset, total)
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return entries[start:end]
}

// buildFileListEntries 从 user 桶目录条目构建文件信息列表并附加 checksum。
// 列表根在 user 桶内（功能桶与 .checksums.json 均不在其下），无需内部目录过滤。
// csMap 来自 per-tenant store（key 为相对租户根的 rel，如 user/dir/f.txt）；
// subdir 为相对 user 桶的子路径，checksum key = "user/" + subdir/entryName。
func (h *Handlers) buildFileListEntries(entries []os.DirEntry, csMap map[string]string, subdir string) []fileInfo {
	allFiles := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			allFiles = append(allFiles, fileInfo{
				Name:  e.Name(),
				IsDir: true,
			})
			continue
		}
		// 任务 8 O-2：分块在途临时文件（.inflight-<hash16>-<upload_id>.part）是服务端内部
		// 文件，列表不对外可见（complete 后随会话清理；中断残留由会话过期清理）。
		if isInflightTempName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			h.logger.Warn("读取文件信息失败，跳过", "name", e.Name(), "error", err)
			continue
		}
		fi := fileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		}
		relName := e.Name()
		if subdir != "" {
			relName = filepath.ToSlash(filepath.Join(subdir, e.Name()))
		}
		if cs, ok := csMap["user/"+relName]; ok {
			fi.Checksum = cs
		}
		allFiles = append(allFiles, fi)
	}
	return allFiles
}

// listFiles 处理 GET /api/files。
// 多卷（任务 5）：owner 视图逐卷 ReadDir 聚合（默认卷优先），文件条目带 volume 字段；
// 目录条目为逻辑目录（可跨卷并存）只列一次、Volume 为空。?volume= 存在时只列指定卷
// （未知卷名或不在 owner 视图 → 404，fail-closed）。checksum 每卷各自读（统一默认卷 store，
// key 为 rel——AD-4 下 rel 视图内唯一，默认卷 checksum 表即全量，无需逐卷查）。
func (h *Handlers) listFiles(w http.ResponseWriter, r *http.Request) {
	// 分页参数
	offset, limit := parsePagination(r)

	// 排序参数
	sortBy := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")
	if sortOrder != "desc" {
		sortOrder = "asc"
	}
	// 归一 owner（空 → anonymous）：列表/写路径同键，未认证请求归属 anonymous。ACL 视图判定与
	// 路径探测必须用归一后的 owner——防 owner="" 以空串参与 ACL（对 deny+黑名单卷误放行）或建 "" 目录。
	owner := normalizeOwner(ownerFromRequest(r))
	subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/")

	// 旧装配路径（volSet nil）：单卷唯一根，走既有 resolveListDir + os.ReadDir（零回归）。
	if h.volSet == nil {
		targetDir, ok := h.resolveListDir(w, r)
		if !ok {
			return
		}
		entries, err := os.ReadDir(targetDir)
		h.logger.Debug("读取目录", "dir", targetDir)
		if os.IsNotExist(err) {
			sendJSONResponse(w, listResponse{Files: []fileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusOK)
			return
		}
		if err != nil {
			h.logger.Error("读取上传目录失败", "error", err.Error())
			sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusInternalServerError)
			return
		}
		var csMap map[string]string
		if cs := h.checksumStoreFor(owner); cs != nil {
			csMap = cs.GetAll()
		} else {
			csMap = map[string]string{}
		}
		allFiles := h.buildFileListEntries(entries, csMap, subdir)
		sortFileEntries(allFiles, sortBy, sortOrder)
		total := len(allFiles)
		sendJSONResponse(w, listResponse{Files: paginateEntries(allFiles, offset, limit), Total: total, Offset: offset, Limit: limit}, http.StatusOK)
		return
	}

	// 多卷聚合：?volume= 过滤（先 ACL，不在视图 → 404）。
	vols := volume.AllowedVolumes(h.volSet.All(), owner)
	if volFilter := r.URL.Query().Get("volume"); volFilter != "" {
		v, ok := h.volSet.ByName(volFilter)
		if !ok || !v.Authorize(owner) {
			sendJSONResponse(w, listResponse{Files: []fileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusNotFound)
			return
		}
		vols = []volume.Volume{v}
	}
	if len(vols) == 0 {
		// owner 视图为空（全部卷 ACL 拒）：按空目录返回（不可见卷绝不列出，AD-2）。
		sendJSONResponse(w, listResponse{Files: []fileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusOK)
		return
	}

	// 子目录校验（与默认租户路径映射同一规则；功能桶天然不可枚举）。
	rel, ok := h.listRelForOwner(owner, subdir)
	if !ok {
		sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
		return
	}

	// 默认卷 checksum store 快照（rel 视图唯一，默认卷表即全量）。
	var csMap map[string]string
	if cs := h.checksumStoreFor(owner); cs != nil {
		csMap = cs.GetAll()
	} else {
		csMap = map[string]string{}
	}

	var allFiles []fileInfo
	seenDirs := make(map[string]bool)
	for _, v := range vols {
		tnt := h.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue
		}
		entries, err := tnt.Root().ReadDir(rel)
		if os.IsNotExist(err) {
			continue // 该卷无此子目录（正常：换卷目录不一定每卷都有）
		}
		if err != nil {
			h.logger.Warn("读取卷目录失败", "volume", v.Name, "dir", rel, "error", err)
			continue
		}
		for _, e := range h.buildFileListEntries(entries, csMap, subdir) {
			if e.IsDir {
				// 目录条目：聚合逻辑目录只列一次（跨卷并存），不绑定单卷。
				if seenDirs[e.Name] {
					continue
				}
				seenDirs[e.Name] = true
				allFiles = append(allFiles, e)
				continue
			}
			e.Volume = v.Name
			allFiles = append(allFiles, e)
		}
	}
	sortFileEntries(allFiles, sortBy, sortOrder)
	total := len(allFiles)
	sendJSONResponse(w, listResponse{Files: paginateEntries(allFiles, offset, limit), Total: total, Offset: offset, Limit: limit}, http.StatusOK)
}

// listRelForOwner 返回 owner 列表目标目录在租户根内的 rel（user 桶，空 subdir 时 = "user"）。
// 子目录经 ValidateFilePath + UserRel 校验（与 resolveListDir 同规则）；失败返回 ok=false。
func (h *Handlers) listRelForOwner(owner, subdir string) (string, bool) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return "", false
	}
	if subdir == "" {
		return tnt.UserRoot(), true
	}
	if _, err := ValidateFilePath(subdir); err != nil {
		return "", false
	}
	rel, ok := tnt.UserRel(subdir)
	return rel, ok
}

// searchFiles 处理 GET /api/files/search?q=keyword。
// 递归搜索请求者租户 user 桶下文件名包含 q 的文件，不区分大小写。
// 多卷（T6b）：按 owner 卷视图逐卷递归搜索（不只默认卷），文件条目带 volume 字段；
// 目录条目为逻辑目录（可跨卷并存）只列一次、Volume 空。默认卷被 ACL 排除则不搜默认卷
// （不泄默认卷遗留元数据）。
func (h *Handlers) searchFiles(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
		return
	}
	qLower := strings.ToLower(q)

	owner := normalizeOwner(ownerFromRequest(r))
	// 一次性快照（见 listFiles #8 结论注释，勿再分析）：per-tenant store，key 为相对租户根的 rel。
	var csMap map[string]string
	if cs := h.checksumStoreFor(owner); cs != nil {
		csMap = cs.GetAll()
	} else {
		csMap = map[string]string{}
	}

	if h.volSet == nil {
		// 旧装配路径：单卷唯一根（搜索根 = user 桶绝对路径；功能桶天然不参与搜索）。
		tnt := h.tenantFor(owner)
		if tnt == nil || tnt.Root() == nil {
			sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
			return
		}
		searchRoot, ok := tnt.Root().Abs(tnt.UserRoot())
		if !ok {
			sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
			return
		}
		var results []fileInfo
		h.collectSearchResults(searchRoot, qLower, csMap, "", &results, make(map[string]bool))
		resp := listResponse{Files: results, Total: len(results), Offset: 0, Limit: len(results)}
		sendJSONResponse(w, resp, http.StatusOK)
		return
	}

	// 多卷：owner 视图逐卷搜索。
	vols := volume.AllowedVolumes(h.volSet.All(), owner)
	if len(vols) == 0 {
		sendJSONResponse(w, listResponse{Files: []fileInfo{}, Total: 0, Offset: 0, Limit: 0}, http.StatusOK)
		return
	}
	var results []fileInfo
	seenDirs := make(map[string]bool)
	for _, v := range vols {
		// 只搜 user 桶存在的卷（只读探测，不创建租户目录——搜索是 GET 无副作用）。
		exists, err := h.volumeFileExists(v.Name, owner, "user")
		if err != nil || !exists {
			continue
		}
		tnt := h.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue
		}
		searchRoot, ok := tnt.Root().Abs(tnt.UserRoot())
		if !ok {
			continue
		}
		h.collectSearchResults(searchRoot, qLower, csMap, v.Name, &results, seenDirs)
	}
	resp := listResponse{Files: results, Total: len(results), Offset: 0, Limit: len(results)}
	sendJSONResponse(w, resp, http.StatusOK)
}

// collectSearchResults 递归搜索请求者租户 user 桶下文件名包含 queryLower 的文件。
// volumeName 非空时为多卷搜索结果（文件条目带该卷名）；seenDirs 跨卷去重逻辑目录条目。
func (h *Handlers) collectSearchResults(rootsDir, queryLower string, csMap map[string]string, volumeName string, results *[]fileInfo, seenDirs map[string]bool) {
	_ = filepath.WalkDir(rootsDir, func(path string, d fs.DirEntry, err error) error {
		return h.searchWalkDirCallback(rootsDir, path, d, err, queryLower, csMap, volumeName, results, seenDirs)
	})
}

// searchWalkDirCallback 是 collectSearchResults 中 filepath.WalkDir 的回调函数。
func (h *Handlers) searchWalkDirCallback(rootsDir, path string, d fs.DirEntry, err error, queryLower string, csMap map[string]string, volumeName string, results *[]fileInfo, seenDirs map[string]bool) error {
	if err != nil {
		h.logger.Warn("搜索时访问路径失败", "path", path, "error", err)
		return nil
	}
	rel, _ := filepath.Rel(rootsDir, path)
	if rel == "." {
		return nil
	}
	// 搜索根在 user 桶内（功能桶与 .checksums.json 均不在其下），无需内部目录过滤。
	if isInflightTempName(d.Name()) {
		// 任务 8 O-2：分块在途临时文件（.inflight-<hash16>-<upload_id>.part）不参与
		// 搜索（服务端内部文件，对外不可见；与列表过滤语义一致）。
		return nil
	}
	if !strings.Contains(strings.ToLower(d.Name()), queryLower) {
		return nil
	}
	if d.IsDir() {
		// 目录条目：聚合逻辑目录只列一次（跨卷并存），不绑定单卷。
		name := filepath.ToSlash(rel)
		if seenDirs[name] {
			return nil
		}
		seenDirs[name] = true
		*results = append(*results, fileInfo{
			Name:  name,
			IsDir: true,
		})
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return nil
	}
	fi := fileInfo{
		Name:    filepath.ToSlash(rel),
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
		Volume:  volumeName,
	}
	if cs, ok := csMap["user/"+filepath.ToSlash(rel)]; ok {
		fi.Checksum = cs
	}
	*results = append(*results, fi)
	return nil
}

// verifyFileWithChecksum 验证文件 SHA-256 checksum 是否匹配。
func verifyFileWithChecksum(filePath, expectedChecksum string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	return verifyChecksum(expectedChecksum, f)
}
