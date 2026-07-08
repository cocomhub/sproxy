// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
		_, _ = fmt.Sscanf(o, "%d", &offset)
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		_, _ = fmt.Sscanf(l, "%d", &limit)
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	return
}

// resolveListDir 处理 listFiles 的 subdir 参数，返回目标目录。
func (h *Handlers) resolveListDir(w http.ResponseWriter, r *http.Request) (targetDir string, ok bool) {
	cfg := h.cfgPtr.Load()
	targetDir = cfg.UploadsDir
	if subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/"); subdir != "" {
		if _, err := ValidateFilePath(subdir); err != nil {
			h.logger.Warn("无效的子目录", "subdir", subdir, "error", err.Error())
			sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
			return "", false
		}
		targetDir = h.safePath(subdir)
		if targetDir == "" {
			h.logger.Warn("无效的子目录路径", "subdir", subdir)
			sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
			return "", false
		}
	}
	return targetDir, true
}

// sortFileEntries 按指定字段和顺序排序文件条目。
func sortFileEntries(entries []fileInfo, sortBy, sortOrder string) {
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
func (h *Handlers) buildFileListEntries(entries []os.DirEntry, csMap map[string]string, subdir string) []fileInfo {
	allFiles := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".checksums.json" || e.Name() == chunkedDirName || e.Name() == versionsDirName || e.Name() == cloudDirName || e.Name() == ".__downloads__" {
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
			continue
		}
		fi := fileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		}
		relName := e.Name()
		if subdir != "" {
			cleaned, _ := ValidateFilePath(subdir)
			relName = filepath.ToSlash(filepath.Join(cleaned, e.Name()))
		}
		if cs, ok := csMap[relName]; ok {
			fi.Checksum = cs
		}
		allFiles = append(allFiles, fi)
	}
	return allFiles
}

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
	h.logger.Info("读取目录", "dir", targetDir)
	if os.IsNotExist(err) {
		sendJSONResponse(w, listResponse{Files: []fileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusOK)
		return
	}
	if err != nil {
		h.logger.Error("读取上传目录失败", "error", err.Error())
		sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusInternalServerError)
		return
	}

	csMap := h.checksumStore.GetAll()

	// 收集所有条目（跳过内部目录）
	subdir := r.URL.Query().Get("subdir")
	allFiles := h.buildFileListEntries(entries, csMap, subdir)

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
		sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
		return
	}
	qLower := strings.ToLower(q)

	cfg := h.cfgPtr.Load()
	csMap := h.checksumStore.GetAll()

	results := h.collectSearchResults(cfg.UploadsDir, qLower, csMap)
	sendJSONResponse(w, map[string]any{"files": results}, http.StatusOK)
}

// collectSearchResults 递归搜索 uploads_dir 下文件名包含 queryLower 的文件。
func (h *Handlers) collectSearchResults(rootsDir, queryLower string, csMap map[string]string) []fileInfo {
	var results []fileInfo
	_ = filepath.WalkDir(rootsDir, func(path string, d fs.DirEntry, err error) error {
		return h.searchWalkDirCallback(rootsDir, path, d, err, queryLower, csMap, &results)
	})
	return results
}

// searchWalkDirCallback 是 collectSearchResults 中 filepath.WalkDir 的回调函数。
func (h *Handlers) searchWalkDirCallback(rootsDir, path string, d fs.DirEntry, err error, queryLower string, csMap map[string]string, results *[]fileInfo) error {
	if err != nil {
		return nil
	}
	rel, _ := filepath.Rel(rootsDir, path)
	if rel == "." {
		return nil
	}
	if d.Name() == ".checksums.json" || d.Name() == chunkedDirName || d.Name() == versionsDirName || d.Name() == cloudDirName || d.Name() == ".__downloads__" {
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
	if cs, ok := csMap[filepath.ToSlash(rel)]; ok {
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
