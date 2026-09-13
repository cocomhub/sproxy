// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/pathguard"
)

// read.go 是文件服务**只读面**（列表 / 搜索 / 下载 / stat）的处理器与它们的私有辅助，
// 自 `pkg/server/{list_handler,download_handler}.go` 平铺迁入（接收者由 *Handlers 改为
// *Service，方法体只换接收者与限定名）。
//
// 与 chunked_download.go 的分工：那边是分块下载（Range 由客户端按 offset/length 指定），
// 这边是整文件下载（Range 由 http.ServeContent 处理）与元信息探测；两者共用同一份
// ResolveDownloadPath 接缝与 writeDownloadPathError / checksumStoreForRead。
//
// 留在装配层（pkg/server）的部分：`/download` 与 `/api/files/stat` 的**路径解析**
// （resolveDownloadPath：kind 白名单 + 卷读定位 + 云任务归属校验，见接缝
// DownloadPaths 能力）。本文件只消费解析结果，不自行解析请求路径。

// headerFileMTime 是文件元信息响应头（下载 / stat 读，upload 写）。
// 写面迁入后本包是本常量的**唯一定义**（pkg/server 侧的同名常量已随上传族删除，本包不再
// 存在第二份定义 ⇒ 无跨侧漂移面）。
const headerFileMTime = "X-File-MTime"

// FileInfo 是文件列表中的条目结构。
type FileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"` // UnixNano
	IsDir    bool   `json:"is_dir"`   // 是否为目录
	// Volume 是条目所在卷名（多卷聚合列表新增字段；向后兼容——旧客户端忽略未知键。
	// 目录条目为聚合视图下的逻辑目录（可跨卷并存），不绑定单卷，Volume 为空）。
	Volume string `json:"volume"`
}

// ListResponse 是文件列表的响应结构（GET /api/files 与 GET /api/files/search 共用）。
type ListResponse struct {
	Files  []FileInfo `json:"files"`
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

// sortFileEntries 按指定字段和顺序排序文件条目。
func sortFileEntries(entries []FileInfo, sortBy, sortOrder string) {
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
func paginateEntries(entries []FileInfo, offset, limit int) []FileInfo {
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
func (s *Service) buildFileListEntries(entries []os.DirEntry, csMap map[string]string, subdir string) []FileInfo {
	allFiles := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			allFiles = append(allFiles, FileInfo{
				Name:  e.Name(),
				IsDir: true,
			})
			continue
		}
		// 任务 8 O-2：分块在途临时文件（.inflight-<hash16>-<upload_id>.part）是服务端内部
		// 文件，列表不对外可见（complete 后随会话清理；中断残留由会话过期清理）。
		if IsInflightTempName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			s.rt.logger().Warn("读取文件信息失败，跳过", "name", e.Name(), "error", err)
			continue
		}
		fi := FileInfo{
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

// ListFiles 处理 GET /api/files。
// 多卷（任务 5）：owner 视图逐卷 ReadDir 聚合（默认卷优先），文件条目带 volume 字段；
// 目录条目为逻辑目录（可跨卷并存）只列一次、Volume 为空。?volume= 存在时只列指定卷
// （未知卷名或不在 owner 视图 → 404，fail-closed）。checksum 每卷各自读（统一默认卷 store，
// key 为 rel——AD-4 下 rel 视图内唯一，默认卷 checksum 表即全量，无需逐卷查）。
func (s *Service) ListFiles(w http.ResponseWriter, r *http.Request) {
	offset, limit := parsePagination(r)
	sortOrder := r.URL.Query().Get("order")
	if sortOrder != "desc" {
		sortOrder = "asc"
	}
	res, err := s.List(ListQuery{
		Owner:     s.rt.actorOf(r),
		VolName:   r.URL.Query().Get("volume"),
		Subdir:    r.URL.Query().Get("subdir"),
		SortBy:    r.URL.Query().Get("sort"),
		SortOrder: sortOrder,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		writeListError(w, s, err, offset, limit)
		return
	}
	// ListResult 与 ListResponse 字段一一对应（见 read_ops.go 的注释），直接转换以保持单一形状。
	s.sendJSON(w, ListResponse(res), http.StatusOK)
}

// writeListError 把列表域方法的失败映射为既有响应（**逐字保留历史形状**）：
//
//   - 404（?volume= 未知/不在视图）：带请求的 offset/limit；
//   - 其他带状态码的失败（400：subdir 非法 / owner 不可用）：**只回 files:[]**（total/offset/limit
//     全为 0）——这是历史形状的既有不对称，改它属于契约变更，需单独决策；
//   - 无状态码的普通错误（旧装配路径读目录失败）：`{"files":[]}` + 500。
func writeListError(w http.ResponseWriter, s *Service, err error, offset, limit int) {
	if he := asHTTPError(err); he != nil {
		if he.Status == http.StatusNotFound {
			s.sendJSON(w, ListResponse{Files: []FileInfo{}, Total: 0, Offset: offset, Limit: limit}, he.Status)
			return
		}
		s.sendJSON(w, ListResponse{Files: []FileInfo{}}, he.Status)
		return
	}
	s.sendJSON(w, map[string]any{"files": []FileInfo{}}, http.StatusInternalServerError)
}

// listRelForOwner 返回 owner 列表目标目录在租户根内的 rel（user 桶，空 subdir 时 = "user"）。
// 子目录经 ValidateFilePath + UserRel 校验（与 resolveListDir 同规则）；失败返回 ok=false。
func (s *Service) listRelForOwner(owner, subdir string) (string, bool) {
	tnt := s.rt.tenantOf(owner)
	if tnt == nil || tnt.Root() == nil {
		return "", false
	}
	if subdir == "" {
		return tnt.UserRoot(), true
	}
	if _, err := pathguard.ValidateFilePath(subdir); err != nil {
		return "", false
	}
	rel, ok := tnt.UserRel(subdir)
	return rel, ok
}

// SearchFiles 处理 GET /api/files/search?q=keyword。
// 递归搜索请求者租户 user 桶下文件名包含 q 的文件，不区分大小写。
// 多卷（T6b）：按 owner 卷视图逐卷递归搜索（不只默认卷），文件条目带 volume 字段；
// 目录条目为逻辑目录（可跨卷并存）只列一次、Volume 空。默认卷被 ACL 排除则不搜默认卷
// （不泄默认卷遗留元数据）。
func (s *Service) SearchFiles(w http.ResponseWriter, r *http.Request) {
	res, err := s.Search(SearchQuery{Owner: s.rt.actorOf(r), Query: r.URL.Query().Get("q")})
	if err != nil {
		// 既有形状：搜索失败一律只回 files:[]（search 路径没有 404 分支）。
		s.sendJSON(w, ListResponse{Files: []FileInfo{}}, asHTTPError(err).Status)
		return
	}
	// ListResult 与 ListResponse 字段一一对应（见 read_ops.go 的注释），直接转换以保持单一形状。
	s.sendJSON(w, ListResponse(res), http.StatusOK)
}

// collectSearchResults 递归搜索请求者租户 user 桶下文件名包含 queryLower 的文件。
// volumeName 非空时为多卷搜索结果（文件条目带该卷名）；seenDirs 跨卷去重逻辑目录条目。
func (s *Service) collectSearchResults(rootsDir, queryLower string, csMap map[string]string, volumeName string, results *[]FileInfo, seenDirs map[string]bool) {
	_ = filepath.WalkDir(rootsDir, func(path string, d fs.DirEntry, err error) error {
		return s.searchWalkDirCallback(rootsDir, path, d, err, queryLower, csMap, volumeName, results, seenDirs)
	})
}

// searchWalkDirCallback 是 collectSearchResults 中 filepath.WalkDir 的回调函数。
func (s *Service) searchWalkDirCallback(rootsDir, path string, d fs.DirEntry, err error, queryLower string, csMap map[string]string, volumeName string, results *[]FileInfo, seenDirs map[string]bool) error {
	if err != nil {
		s.rt.logger().Warn("搜索时访问路径失败", "path", path, "error", err)
		return nil
	}
	rel, _ := filepath.Rel(rootsDir, path)
	if rel == "." {
		return nil
	}
	// 搜索根在 user 桶内（功能桶与 .checksums.json 均不在其下），无需内部目录过滤。
	if IsInflightTempName(d.Name()) {
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
		*results = append(*results, FileInfo{
			Name:  name,
			IsDir: true,
		})
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return nil
	}
	fi := FileInfo{
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

// volumeFileExists 探测指定卷上 owner 的 rel 是否已存在（只读，不创建租户目录）。
// 路径 = <卷根>/<owner>/<rel>（rel 含 user/ 前缀）。
// 语义 fail-closed：卷未知 → (false, nil)；stat 确证不存在（fs.ErrNotExist）→ (false, nil)；
// 其它 stat 错误（权限/IO 等）→ (false, err)，调用方按 500 处理——不得把「探测失败」当
// 「不存在」继续写（防权限/IO 错误下误覆盖判断）。
//
// 本函数自 pkg/server/volumes.go **原样下沉**（函数体逐字未改，仅接收者由 `h` 的
// `volSet` 字段改为形参 `vs VolumeSet`）：它只用**已注入**的卷集合做只读探测，
// 不含 Handlers 私有状态——属**纯计算**，按接缝判据（纯计算不进接缝）下沉为本地实现。
// 两份实现的等价性由 `pkg/server/helper_impl_drift_test.go` 的源码级断言守卫。
func volumeFileExists(vs VolumeSet, volName, owner, rel string) (bool, error) {
	owner = normalizeOwner(owner)
	rt := vs.Root(volName)
	if rt == nil {
		return false, nil
	}
	_, err := rt.Stat(owner + "/" + rel)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("探测卷 %q 文件状态失败: %w", volName, err)
}

// countingWriter 包装 http.ResponseWriter 并追踪实际写入的字节数。
// 用于 http.ServeContent 写入后记录实际传输字节（而非 Content-Length）。
type countingWriter struct {
	http.ResponseWriter
	count atomic.Int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(p)
	cw.count.Add(int64(n))
	return n, err
}

// Download 处理 GET /download（整文件下载，Range 由 http.ServeContent 处理）。
// 路径解析（kind 白名单 / 跨卷读定位 / 云任务归属校验）由装配层经 DownloadPaths 能力
// 完成后交进来，本处理器只消费解析结果。
func (s *Service) Download(w http.ResponseWriter, r *http.Request) {
	dp, err := s.rt.resolveDownloadPath(r)
	if err != nil {
		s.writeDownloadPathError(w, err)
		return
	}

	of, err := s.OpenPath(dp)
	if err != nil {
		if he := asHTTPError(err); he != nil {
			s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgOpenFileFailed}, http.StatusInternalServerError)
		return
	}
	defer func() { _ = of.File.Close() }()

	w.Header().Set("Content-Disposition", formatContentDisposition(dp.Filename))
	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set("Accept-Ranges", "bytes")
	if of.Checksum != "" {
		w.Header().Set(headerFileChecksum, of.Checksum)
	}
	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", of.Info.ModTime().UnixNano()))

	// 使用 http.ServeContent 替代 http.ServeFile：
	//   - 自动处理 Range header（返回 206 + Content-Range，旧客户端不带 Range 仍 200 全量）
	//   - 不会根据扩展名嗅探并覆盖已设置的 Content-Type（同步修复缺陷 #12）
	// 域侧只保证 io.ReadCloser（远程读面只需流式读）；Range/206 需要随机读，由 HTTP 层断言。
	// 进程内 Download 的句柄恒为 *os.File（可断言成功）；本分支只是防御。
	seeker, ok := of.File.(io.ReadSeeker)
	if !ok {
		s.rt.logger().Error("下载句柄不支持随机读（无法处理 Range）", "file_name", dp.Filename)
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgOpenFileFailed}, http.StatusInternalServerError)
		return
	}
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, of.Info.Name(), of.Info.ModTime(), seeker)
	if s.rt.metricsRecorder() != nil {
		s.rt.metricsRecorder().RecordDownload(cw.count.Load())
	}
}

// Stat 处理 HEAD /api/files/stat?filename=<name>[&kind=cloud_archive]。
// 通过响应头 X-File-Size、X-File-Checksum、X-File-MTime（UnixNano）返回元信息。
// kind 为空走普通文件路径；kind=cloud_archive 解析归档目录（供分块下载前 stat）。
// 文件不存在返回 404；不返回响应体。
func (s *Service) Stat(w http.ResponseWriter, r *http.Request) {
	dp, err := s.rt.resolveDownloadPath(r)
	if err != nil {
		s.writeHTTPPathError(w, err)
		return
	}
	st, err := s.StatPath(dp)
	if err != nil {
		if he := asHTTPError(err); he != nil {
			http.Error(w, he.Message, he.Status)
			return
		}
		http.Error(w, "stat error", http.StatusInternalServerError)
		return
	}
	if st.IsDir {
		w.Header().Set("X-File-IsDir", "true")
	}
	w.Header().Set("X-File-Size", fmt.Sprintf("%d", st.Size))
	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", st.MTime))
	if st.Checksum != "" {
		w.Header().Set(headerFileChecksum, st.Checksum)
	}
	w.WriteHeader(http.StatusOK)
}

// writeHTTPPathError 把 ResolveDownloadPath 返回的错误映射为统一 http.Error 响应
// （供 stat 等非 JSON handler 使用）。
// 与 pkg/server.writeHTTPPathError 语义一致：带 HTTP 状态码的解析错误（装配层经
// HTTPError 传入，对应 pkg/server 的 *downloadPathError）按其状态码与文案回包；其余一律
// 400 + invalid filename。
func (s *Service) writeHTTPPathError(w http.ResponseWriter, err error) {
	var he *HTTPError
	if errors.As(err, &he) {
		http.Error(w, he.Message, he.Status)
		return
	}
	http.Error(w, "invalid filename", http.StatusBadRequest)
}
