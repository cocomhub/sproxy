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
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
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
// DownloadPathResolver 能力）。本文件只消费解析结果，不自行解析请求路径。

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
// root 为租户根（可为 nil：无卷上下文），**加密卷**下用 `LogicalSize` 报明文长度
// （Stat 是密文长度，会让列表 size 与下载实际字节不一致——第 5 轮对抗评审）。
func (s *Service) buildFileListEntries(entries []os.DirEntry, csMap map[string]string, subdir string, root *storage.Root) []FileInfo {
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
		relName := e.Name()
		if subdir != "" {
			relName = filepath.ToSlash(filepath.Join(subdir, e.Name()))
		}
		fi := FileInfo{
			Name:    e.Name(),
			Size:    logicalSizeFor(root, "user/"+relName, info),
			ModTime: info.ModTime().UnixNano(),
		}
		if cs, ok := csMap["user/"+relName]; ok {
			fi.Checksum = cs
		}
		allFiles = append(allFiles, fi)
	}
	return allFiles
}

// logicalSizeFor 返回条目的**用户可见**大小：at-rest 加密卷取明文长度（`info.Size()`
// 是密文长度，会让列表/X-File-Size/分块总量与 `/download` 实际下发字节不一致）。
// 探测失败回落盘上大小（保持旧行为，不因元信息探测失败拒绝服务）。
func logicalSizeFor(root *storage.Root, rel string, info os.FileInfo) int64 {
	if root == nil || info.IsDir() || !root.IsEncrypted() {
		return info.Size()
	}
	if ls, lerr := root.LogicalSize(rel); lerr == nil {
		return ls
	}
	return info.Size()
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
// 子目录经 ValidateFilePath + UserRel 校验（与 List 的 subdir 分支同规则）；失败返回 ok=false。
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

// SearchFiles 处理 GET /api/files/search?q=keyword[&tag=label]。
// 递归搜索请求者租户 user 桶下文件名包含 q 的文件，不区分大小写；tag 非空时按标签
// 精确过滤（roadmap 11.10-④，与 q AND 组合；q 为空 + tag 非空 = 按标签过滤全部）。
// 多卷（T6b）：按 owner 卷视图逐卷递归搜索（不只默认卷），文件条目带 volume 字段；
// 目录条目为逻辑目录（可跨卷并存）只列一次、Volume 空。默认卷被 ACL 排除则不搜默认卷
// （不泄默认卷遗留元数据）。
func (s *Service) SearchFiles(w http.ResponseWriter, r *http.Request) {
	res, err := s.Search(SearchQuery{
		Owner: s.rt.actorOf(r),
		Query: r.URL.Query().Get("q"),
		Tag:   r.URL.Query().Get("tag"),
	})
	if err != nil {
		// 既有形状：搜索失败一律只回 files:[]（search 路径没有 404 分支）。
		s.sendJSON(w, ListResponse{Files: []FileInfo{}}, asHTTPError(err).Status)
		return
	}
	// ListResult 与 ListResponse 字段一一对应（见 read_ops.go 的注释），直接转换以保持单一形状。
	s.sendJSON(w, ListResponse(res), http.StatusOK)
}

// collectSearchResults 及其回调 searchWalkDirCallback 是**旧 WalkDir 搜索实现**，
// 已由 search_index.go 的增量索引取代（roadmap P0）。保留本文件定义曾承担的无副作用
// 只读语义不再被调用——整体删除（git 历史可回溯），避免 deadcode 门禁拦截。
//
// （删除后本文件不再引用 fs/filepath 的 WalkDir，相关 import 已随之移除。）

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
// 路径解析（kind 白名单 / 跨卷读定位 / 云任务归属校验）由装配层经 DownloadPathResolver 能力
// 完成后交进来，本处理器只消费解析结果。
func (s *Service) Download(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	dp, err := s.rt.resolveDownloadPath(r)
	if err != nil {
		if mr := s.rt.metricsRecorder(); mr != nil {
			mr.RecordVolumeIO("", "download", time.Since(start), false)
		}
		s.writeDownloadPathError(w, err)
		return
	}

	// **302 直链（2026-10-05 B 态）**：装配层已判定明文外部卷未私密 → RedirectURL
	// 非空。流量不经服务端，直接 302 到后端直链；指标按卷记成功。
	if dp.RedirectURL != "" {
		if mr := s.rt.metricsRecorder(); mr != nil {
			mr.RecordVolumeIO(dp.VolumeName, "download", time.Since(start), true)
		}
		http.Redirect(w, r, dp.RedirectURL, http.StatusFound)
		return
	}

	of, err := s.OpenPath(r.Context(), dp)
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
	// 上传管线扩展（roadmap 2.3 P2）：?transform=thumb&width=N 时按需生成派生内容（原文件不动）。
	if tq := r.URL.Query().Get("transform"); tq != "" {
		s.serveTransform(w, r, dp, of, tq)
		return
	}
	seeker, ok := of.File.(io.ReadSeeker)
	if !ok {
		s.rt.logger().Error("下载句柄不支持随机读（无法处理 Range）", "file_name", dp.Filename)
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgOpenFileFailed}, http.StatusInternalServerError)
		return
	}
	// 带宽限速（roadmap §6 P1）：ResponseWriter 按 owner 桶包一层限速 writer（含字节计数）。
	owner := normalizeOwner(s.rt.actorOf(r))
	cw := &countingWriter{ResponseWriter: w}
	limited := limitResponseWriter(s.rt.bandwidthLimiter(), owner, cw)
	// 第 5 轮对抗评审 P1：`meta.VerifyReadSeeker` 在损坏时于 Read 返回错误，但
	// `http.ServeContent` **吞掉** io.Copy 错误且响应头（含 Content-Length）已发出 →
	// 客户端只收到短包，监控还硬编码 success=true、无任何日志。此处记录首个非 EOF
	// 读取错误，随后**主动中断连接**（对齐 s3_server.go 的 ErrAbortHandler）。
	tracked := &firstErrReadSeeker{inner: seeker}
	http.ServeContent(limited, r, of.Info.Name(), of.Info.ModTime(), tracked)
	s.finishDownload(r, dp, start, cw, tracked)
}

// finishDownload 收尾 /download：校验/读取失败 → 中断连接 + 记录失败（第 5 轮对抗评审
// P1：ServeContent 吞掉错误，响应头已发只会静默截断且指标恒 true）；成功 → 记录计量。
func (s *Service) finishDownload(r *http.Request, dp DownloadPath, start time.Time, cw *countingWriter, tracked *firstErrReadSeeker) {
	if tracked.err != nil {
		s.rt.logger().Warn("下载流校验失败：中断连接（不发损坏内容）",
			"file_name", dp.Filename, "volume", dp.VolumeName, "written", cw.count.Load(), "error", tracked.err.Error())
		if s.rt.metricsRecorder() != nil {
			s.rt.metricsRecorder().RecordVolumeIO(dp.VolumeName, "download", time.Since(start), false)
		}
		panic(http.ErrAbortHandler)
	}
	if s.rt.metricsRecorder() == nil {
		return
	}
	s.rt.metricsRecorder().RecordDownload(cw.count.Load())
	// 计量报告 owner 维度（roadmap 11.10-⑩ 片 2）：整文件下载按请求主体记 per-owner 字节。
	if owner := s.rt.actorOf(r); owner != "" {
		s.rt.metricsRecorder().RecordDownloadForOwner(owner, cw.count.Load())
	}
	s.rt.metricsRecorder().RecordVolumeIO(dp.VolumeName, "download", time.Since(start), true)
}

// firstErrReadSeeker 记录底层读的**首个非 EOF 错误**并原样返回（Read/Seek 直透）。
// 供 /download 在 ServeContent 返回后判定「是否因校验/读错而截断」，从而中断连接并
// 把指标记为失败（ServeContent 本身不暴露该错误）。
// 并发安全：ServeContent 对同一 seeker 串行 Read（单 goroutine），无需锁。
type firstErrReadSeeker struct {
	inner io.ReadSeeker
	err   error
}

func (f *firstErrReadSeeker) Read(p []byte) (int, error) {
	n, err := f.inner.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && f.err == nil {
		f.err = err
	}
	return n, err
}

func (f *firstErrReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return f.inner.Seek(offset, whence)
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
	st, err := s.StatPath(r.Context(), dp)
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
	if he, ok := errors.AsType[*HTTPError](err); ok {
		http.Error(w, he.Message, he.Status)
		return
	}
	http.Error(w, "invalid filename", http.StatusBadRequest)
}
