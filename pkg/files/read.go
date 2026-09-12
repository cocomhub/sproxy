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
	"github.com/cocomhub/sproxy/pkg/volume"
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

// resolveListDir 处理 ListFiles 的 subdir 参数，返回请求者租户 user 桶内的目标目录
// 绝对路径（供 os.ReadDir）。默认根 = user 桶绝对路径；subdir 经 UserRel 映射到
// user/<subdir>（功能桶名首段作为用户路径合法，解析到 user 桶内，功能桶天然不可枚举）。
// ValidateFilePath 格式校验保留（防穿越/非法字符）。
func (s *Service) resolveListDir(w http.ResponseWriter, r *http.Request) (targetDir string, ok bool) {
	tnt := s.rt.tenantOf(s.rt.actorOf(r))
	if tnt == nil || tnt.Root() == nil {
		s.rt.logger().Warn("租户不可用", "owner", s.rt.actorOf(r))
		s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
		return "", false
	}
	root := tnt.Root()
	targetDir, ok = root.Abs(tnt.UserRoot())
	if !ok {
		s.rt.logger().Warn("派生 user 桶路径失败", "owner", s.rt.actorOf(r))
		s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
		return "", false
	}
	if subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/"); subdir != "" {
		if _, err := pathguard.ValidateFilePath(subdir); err != nil {
			s.rt.logger().Warn("无效的子目录", "subdir", subdir, "error", err.Error())
			s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
			return "", false
		}
		rel, uok := tnt.UserRel(subdir)
		if !uok {
			s.rt.logger().Warn("无效的子目录路径", "subdir", subdir)
			s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
			return "", false
		}
		targetDir, ok = root.Abs(rel)
		if !ok {
			s.rt.logger().Warn("子目录路径越界", "subdir", subdir)
			s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
			return "", false
		}
	}
	return targetDir, true
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
	owner := normalizeOwner(s.rt.actorOf(r))
	subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/")

	// 旧装配路径（VolSet nil）：单卷唯一根，走既有 resolveListDir + os.ReadDir（零回归）。
	if s.rt.volSet() == nil {
		targetDir, ok := s.resolveListDir(w, r)
		if !ok {
			return
		}
		entries, err := os.ReadDir(targetDir)
		s.rt.logger().Debug("读取目录", "dir", targetDir)
		if os.IsNotExist(err) {
			s.sendJSON(w, ListResponse{Files: []FileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusOK)
			return
		}
		if err != nil {
			s.rt.logger().Error("读取上传目录失败", "error", err.Error())
			s.sendJSON(w, map[string]any{"files": []FileInfo{}}, http.StatusInternalServerError)
			return
		}
		var csMap map[string]string
		if cs := s.rt.checksumStore(owner); cs != nil {
			csMap = cs.GetAll()
		} else {
			csMap = map[string]string{}
		}
		allFiles := s.buildFileListEntries(entries, csMap, subdir)
		sortFileEntries(allFiles, sortBy, sortOrder)
		total := len(allFiles)
		s.sendJSON(w, ListResponse{Files: paginateEntries(allFiles, offset, limit), Total: total, Offset: offset, Limit: limit}, http.StatusOK)
		return
	}

	// 多卷聚合：?volume= 过滤（先 ACL，不在视图 → 404）。
	vols := volume.AllowedVolumes(s.rt.volSet().All(), owner)
	if volFilter := r.URL.Query().Get("volume"); volFilter != "" {
		v, ok := s.rt.volSet().ByName(volFilter)
		if !ok || !v.Authorize(owner) {
			s.sendJSON(w, ListResponse{Files: []FileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusNotFound)
			return
		}
		vols = []volume.Volume{v}
	}
	if len(vols) == 0 {
		// owner 视图为空（全部卷 ACL 拒）：按空目录返回（不可见卷绝不列出，AD-2）。
		s.sendJSON(w, ListResponse{Files: []FileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusOK)
		return
	}

	// 子目录校验（与默认租户路径映射同一规则；功能桶天然不可枚举）。
	rel, ok := s.listRelForOwner(owner, subdir)
	if !ok {
		s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
		return
	}

	// 默认卷 checksum store 快照（rel 视图唯一，默认卷表即全量）。
	var csMap map[string]string
	if cs := s.rt.checksumStore(owner); cs != nil {
		csMap = cs.GetAll()
	} else {
		csMap = map[string]string{}
	}

	var allFiles []FileInfo
	seenDirs := make(map[string]bool)
	for _, v := range vols {
		tnt := s.rt.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue
		}
		entries, err := tnt.Root().ReadDir(rel)
		if os.IsNotExist(err) {
			continue // 该卷无此子目录（正常：换卷目录不一定每卷都有）
		}
		if err != nil {
			s.rt.logger().Warn("读取卷目录失败", "volume", v.Name, "dir", rel, "error", err)
			continue
		}
		for _, e := range s.buildFileListEntries(entries, csMap, subdir) {
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
	s.sendJSON(w, ListResponse{Files: paginateEntries(allFiles, offset, limit), Total: total, Offset: offset, Limit: limit}, http.StatusOK)
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
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
		return
	}
	qLower := strings.ToLower(q)

	owner := normalizeOwner(s.rt.actorOf(r))
	// 一次性快照（见 ListFiles #8 结论注释，勿再分析）：per-tenant store，key 为相对租户根的 rel。
	var csMap map[string]string
	if cs := s.rt.checksumStore(owner); cs != nil {
		csMap = cs.GetAll()
	} else {
		csMap = map[string]string{}
	}

	if s.rt.volSet() == nil {
		// 旧装配路径：单卷唯一根（搜索根 = user 桶绝对路径；功能桶天然不参与搜索）。
		tnt := s.rt.tenantOf(owner)
		if tnt == nil || tnt.Root() == nil {
			s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
			return
		}
		searchRoot, ok := tnt.Root().Abs(tnt.UserRoot())
		if !ok {
			s.sendJSON(w, ListResponse{Files: []FileInfo{}}, http.StatusBadRequest)
			return
		}
		var results []FileInfo
		s.collectSearchResults(searchRoot, qLower, csMap, "", &results, make(map[string]bool))
		resp := ListResponse{Files: results, Total: len(results), Offset: 0, Limit: len(results)}
		s.sendJSON(w, resp, http.StatusOK)
		return
	}

	// 多卷：owner 视图逐卷搜索。
	vols := volume.AllowedVolumes(s.rt.volSet().All(), owner)
	if len(vols) == 0 {
		s.sendJSON(w, ListResponse{Files: []FileInfo{}, Total: 0, Offset: 0, Limit: 0}, http.StatusOK)
		return
	}
	var results []FileInfo
	seenDirs := make(map[string]bool)
	for _, v := range vols {
		// 只搜 user 桶存在的卷（只读探测，不创建租户目录——搜索是 GET 无副作用）。
		exists, err := volumeFileExists(s.rt.volSet(), v.Name, owner, "user")
		if err != nil || !exists {
			continue
		}
		tnt := s.rt.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue
		}
		searchRoot, ok := tnt.Root().Abs(tnt.UserRoot())
		if !ok {
			continue
		}
		s.collectSearchResults(searchRoot, qLower, csMap, v.Name, &results, seenDirs)
	}
	resp := ListResponse{Files: results, Total: len(results), Offset: 0, Limit: len(results)}
	s.sendJSON(w, resp, http.StatusOK)
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

	// 所有下载 kind 均经租户根打开（root 相对，防符号链接逃逸）。
	file, err := dp.Tenant.Root().Open(dp.Rel)
	if err != nil {
		if os.IsNotExist(err) {
			s.sendJSON(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			s.rt.logger().Error("打开文件失败", "file_name", dp.Filename, "error", err.Error())
			s.sendJSON(w, UploadResponse{Success: false, Message: errMsgOpenFileFailed}, http.StatusInternalServerError)
		}
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		s.rt.logger().Error("stat 文件失败", "file_name", dp.Filename, "error", err.Error())
		s.sendJSON(w, UploadResponse{Success: false, Message: "stat 失败"}, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Disposition", formatContentDisposition(dp.Filename))
	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set("Accept-Ranges", "bytes")

	// 设置 SHA-256 checksum 响应头：优先从 store 读取，回退实时计算。
	// 回退路径优先复用已打开的文件句柄（零额外 I/O），仅当计算成功后才写入缓存。
	// 统一 per-tenant store + 根内相对路径 key（无 owner 前缀，与写端 checksumStoreFor 一致）。
	if csStore, csKey := s.checksumStoreForRead(dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			w.Header().Set(headerFileChecksum, cs)
		} else {
			// 缓存未命中，从已打开文件句柄计算（复用 file，零额外 I/O）
			_, _ = file.Seek(0, io.SeekStart)
			if cs, err := checksumReader(file); err == nil {
				_, _ = file.Seek(0, io.SeekStart)
				csStore.Set(csKey, cs)
				w.Header().Set(headerFileChecksum, cs)
			} else {
				s.rt.logger().Warn("计算文件 checksum 失败", "error", err.Error(), "file_name", dp.Filename)
			}
		}
	}

	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", info.ModTime().UnixNano()))

	// 使用 http.ServeContent 替代 http.ServeFile：
	//   - 自动处理 Range header（返回 206 + Content-Range，旧客户端不带 Range 仍 200 全量）
	//   - 不会根据扩展名嗅探并覆盖已设置的 Content-Type（同步修复缺陷 #12）
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, info.Name(), info.ModTime(), file)
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
	info, err := dp.Tenant.Root().Stat(dp.Rel)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			s.rt.logger().Error("stat 失败", "file_name", dp.Filename, "error", err.Error())
			http.Error(w, "stat error", http.StatusInternalServerError)
		}
		return
	}
	if info.IsDir() {
		w.Header().Set("X-File-IsDir", "true")
	}
	w.Header().Set("X-File-Size", fmt.Sprintf("%d", info.Size()))
	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", info.ModTime().UnixNano()))
	if csStore, csKey := s.checksumStoreForRead(dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			w.Header().Set(headerFileChecksum, cs)
		} else if !info.IsDir() {
			cs, err := fileChecksumRoot(dp.Tenant.Root(), dp.Rel)
			if err == nil {
				w.Header().Set(headerFileChecksum, cs)
			}
		}
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
