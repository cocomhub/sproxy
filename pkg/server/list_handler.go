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
)

// fileInfo 是文件列表中的条目结构。
type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"` // UnixNano
	IsDir    bool   `json:"is_dir"`   // 是否为目录
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

// resolveListDir 处理 listFiles 的 subdir 参数，返回请求者 owner 存储根下的目标目录。
func (h *Handlers) resolveListDir(w http.ResponseWriter, r *http.Request) (targetDir string, ok bool) {
	targetDir = h.ownerUploadsDir(r)
	if subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/"); subdir != "" {
		if _, err := ValidateFilePath(subdir); err != nil {
			h.logger.Warn("无效的子目录", "subdir", subdir, "error", err.Error())
			sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
			return "", false
		}
		targetDir = h.safePathFor(r, subdir)
		if targetDir == "" {
			h.logger.Warn("无效的子目录路径", "subdir", subdir)
			sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
			return "", false
		}
	}
	return targetDir, true
}

// isInternalDir 检查是否为内部管理目录或文件，应跳过列表显示。
// 多租户 owner 隔离后，内部目录只可能作为**首段**出现（uploadsDir/.__versions__，
// 或 uploadsDir/<owner>/.__versions__）；buildFileListEntries 遍历单层（ReadDir），
// 用首段判定即可收敛；深层含 .__ 的普通文件（dir/foo.__bar.txt）不受影响。
// 判断与写入侧守卫 isInternalDirPathPrefix 保持同源（内部目录名集合）。
func isInternalDir(name string) bool {
	// 兼容单层条目名与 .checksums.json 特殊文件。
	if name == ".checksums.json" {
		return true
	}
	return isInternalFirstName(name)
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

// buildFileListEntries 从目录条目构建文件信息列表，排除内部目录并附加 checksum。
// owner 用于 checksum 作用域 key 查找（跨租户同路径独立）。
func (h *Handlers) buildFileListEntries(owner string, entries []os.DirEntry, csMap map[string]string, subdir string) []fileInfo {
	allFiles := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if isInternalDir(e.Name()) {
			continue
		}
		if e.IsDir() {
			allFiles = append(allFiles, fileInfo{
				Name:  e.Name(),
				IsDir: true,
			})
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
		if cs, ok := csMap[checksumStoreKey(owner, relName)]; ok {
			fi.Checksum = cs
		}
		allFiles = append(allFiles, fi)
	}
	return allFiles
}

// listFiles 处理 GET /api/files。
func (h *Handlers) listFiles(w http.ResponseWriter, r *http.Request) {
	// 支持按层级查询：?subdir=path 列出指定子目录，默认列出根目录
	targetDir, ok := h.resolveListDir(w, r)
	if !ok {
		return
	}

	// 分页参数
	offset, limit := parsePagination(r)

	// 排序参数
	sortBy := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")
	if sortOrder != "desc" {
		sortOrder = "asc"
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

	// 收集所有条目（跳过内部目录）
	subdir := r.URL.Query().Get("subdir")
	// getAll 一次快照锁定（审查 #8 结论，勿再分析）：checksum key 形如
	// "owner?/rel-prefix"，逐条 Get 需 N 次加锁且语义等价于一次 Concurrent Map 拷贝；
	// 单目录最多 10000 条（parsePagination limit 上限），GetAll 拷贝成本可忽略。
	csMap := h.checksumStore.GetAll()
	allFiles := h.buildFileListEntries(ownerFromRequest(r), entries, csMap, subdir)
	// 排序
	sortFileEntries(allFiles, sortBy, sortOrder)
	total := len(allFiles)

	// 分页
	files := paginateEntries(allFiles, offset, limit)
	sendJSONResponse(w, listResponse{Files: files, Total: total, Offset: offset, Limit: limit}, http.StatusOK)
}

// searchFiles 处理 GET /api/files/search?q=keyword。
// 递归搜索 uploads_dir 下文件名包含 q 的文件，不区分大小写。
// 返回与 listFiles 相同的 fileInfo 结构。
func (h *Handlers) searchFiles(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		sendJSONResponse(w, listResponse{Files: []fileInfo{}}, http.StatusBadRequest)
		return
	}
	qLower := strings.ToLower(q)

	owner := ownerFromRequest(r)
	csMap := h.checksumStore.GetAll() // 一次性快照（见 listFiles #8 结论注释，勿再分析）
	results := h.collectSearchResults(owner, h.ownerUploadsDir(r), qLower, csMap)
	resp := listResponse{Files: results, Total: len(results), Offset: 0, Limit: len(results)}
	sendJSONResponse(w, resp, http.StatusOK)
}

// collectSearchResults 递归搜索请求者 owner 存储根下文件名包含 queryLower 的文件。
func (h *Handlers) collectSearchResults(owner, rootsDir, queryLower string, csMap map[string]string) []fileInfo {
	var results []fileInfo
	_ = filepath.WalkDir(rootsDir, func(path string, d fs.DirEntry, err error) error {
		return h.searchWalkDirCallback(owner, rootsDir, path, d, err, queryLower, csMap, &results)
	})
	return results
}

// searchWalkDirCallback 是 collectSearchResults 中 filepath.WalkDir 的回调函数。
func (h *Handlers) searchWalkDirCallback(owner, rootsDir, path string, d fs.DirEntry, err error, queryLower string, csMap map[string]string, results *[]fileInfo) error {
	if err != nil {
		h.logger.Warn("搜索时访问路径失败", "path", path, "error", err)
		return nil
	}
	rel, _ := filepath.Rel(rootsDir, path)
	if rel == "." {
		return nil
	}
	// rel 以平台分隔符表达；转正斜杠后取首段做内部目录判定（owner 子目录下的
	// .__versions__/<owner>/ 深层内部目录同样被截断，多租户一致性，审查 #5）。
	if isInternalDirPathPrefix(filepath.ToSlash(rel)) {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if !strings.Contains(strings.ToLower(d.Name()), queryLower) {
		return nil
	}
	if d.IsDir() {
		*results = append(*results, fileInfo{
			Name:  filepath.ToSlash(rel),
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
	}
	if cs, ok := csMap[checksumStoreKey(owner, filepath.ToSlash(rel))]; ok {
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
