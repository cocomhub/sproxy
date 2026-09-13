// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// read_ops.go 是**只读面的域操作 API**：列表 / 搜索 / stat / 打开。
//
// 与 `read.go` 的分工（D-2 的核心约定）：
//
//	read_ops.go  领域逻辑：ACL 视图 → 卷聚合 → 条目标注 → 排序分页 / 打开与元信息
//	read.go      HTTP 薄适配：解析请求参数 → 调域方法 → 写状态码/响应头/JSON
//
// 为什么要有这一层：新表面（Y 一期的跨节点只读、二期的跨节点写）必须**复用**同一份
// 只读语义，而不是"改写 HTTP 请求再打回处理器"。域方法的入参出参里**没有任何 HTTP 类型**
// （除 `DownloadPath`——它是装配层注入的**策略结果**：kind 白名单/跨卷读定位/云任务归属），
// 因此隧道内的远程只读面可以直接调用它们。
//
// 错误语义：域方法用 `*HTTPError{Status, Message}` 表达"应映射成某个 HTTP 状态"的失败
// （与既有 writeHTTPPathError 同一惯例），调用方据此写响应，不自行发明状态码。
package files

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// ListQuery 是列表查询的领域入参。
type ListQuery struct {
	// Owner 是操作主体（空 → anonymous；本方法内经 NormalizeOwner 归一）。
	Owner string
	// VolName 是 `?volume=` 过滤（空 = 全部可见卷；未知/不在视图 → 404）。
	VolName string
	// Subdir 是 user 桶内相对子目录（前导 `/` 容忍；非法 → 400）。
	Subdir string
	// SortBy / SortOrder 是排序参数（SortOrder 非 "desc" 一律按 asc，与既有语义一致）。
	SortBy    string
	SortOrder string
	Offset    int
	Limit     int
}

// ListResult 是列表与搜索共用的领域出参。
//
// Files **恒为非 nil**：空结果必须序列化为 `"files":[]` 而非 `null`——这是既有 HTTP
// 契约的一部分（客户端按数组解码）。
type ListResult struct {
	Files  []FileInfo
	Total  int
	Offset int
	Limit  int
}

// SearchQuery 是搜索的领域入参（搜索无常规分页：Offset/Limit 回填为结果总数）。
type SearchQuery struct {
	Owner string
	// Query 是文件名子串（大小写不敏感）。空（或全空白）→ *HTTPError{400}。
	Query string
}

// FileStat 是单条目元信息的领域出参（不含任何 HTTP 头：由调用方决定怎么对外表达）。
type FileStat struct {
	IsDir    bool
	Size     int64
	MTime    int64  // UnixNano
	Checksum string // 可能为空：checksum 台账未装配，或未命中且实时计算失败
}

// OpenedFile 是打开后的句柄与元信息。
//
// Range / 206 / Content-Range **不在本层**：那由 HTTP 层的 http.ServeContent 承担；
// 隧道内的远程读面直接把 File 流出去即可（对端自行决定要不要 Range）。
type OpenedFile struct {
	File     io.ReadCloser
	Info     os.FileInfo
	Checksum string // 同上：可能为空
}

// List 实现 GET /api/files 的领域逻辑：owner 可见卷 → 逐卷 ReadDir 聚合 → 标注卷名与
// checksum → 排序 → 分页。
//
// 错误语义：
//   - `*HTTPError{400}`：owner 不可用 / 派生 user 桶失败 / subdir 非法（同既有 resolveListDir）
//   - `*HTTPError{404}`：`?volume=` 未知或不在 owner 视图（fail-closed，不泄露存在性）
//   - 其他 error：目录读取失败（仅旧装配路径会走到）
func (s *Service) List(q ListQuery) (ListResult, error) {
	owner := normalizeOwner(q.Owner)
	subdir := strings.TrimPrefix(q.Subdir, "/")
	empty := ListResult{Files: []FileInfo{}, Offset: q.Offset, Limit: q.Limit}

	// 旧装配路径（VolSet nil）：单卷唯一根，唯一差异是目录由 tenantOf 直接派生。
	if s.rt.volSet() == nil {
		tnt := s.rt.tenantOf(owner)
		if tnt == nil || tnt.Root() == nil {
			s.rt.logger().Warn("租户不可用", "owner", owner)
			return empty, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
		}
		root := tnt.Root()
		targetDir, ok := root.Abs(tnt.UserRoot())
		if !ok {
			s.rt.logger().Warn("派生 user 桶路径失败", "owner", owner)
			return empty, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
		}
		if subdir != "" {
			if _, err := pathguard.ValidateFilePath(subdir); err != nil {
				s.rt.logger().Warn("无效的子目录", "subdir", subdir, "error", err.Error())
				return empty, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
			}
			rel, uok := tnt.UserRel(subdir)
			if !uok {
				s.rt.logger().Warn("无效的子目录路径", "subdir", subdir)
				return empty, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
			}
			targetDir, ok = root.Abs(rel)
			if !ok {
				s.rt.logger().Warn("子目录路径越界", "subdir", subdir)
				return empty, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
			}
		}

		entries, err := os.ReadDir(targetDir)
		s.rt.logger().Debug("读取目录", "dir", targetDir)
		if os.IsNotExist(err) {
			return empty, nil
		}
		if err != nil {
			s.rt.logger().Error("读取上传目录失败", "error", err.Error())
			return ListResult{}, err
		}
		allFiles := s.buildFileListEntries(entries, s.checksumSnapshot(owner), subdir)
		sortFileEntries(allFiles, q.SortBy, q.SortOrder)
		return ListResult{
			Files: paginateEntries(allFiles, q.Offset, q.Limit), Total: len(allFiles),
			Offset: q.Offset, Limit: q.Limit,
		}, nil
	}

	// 多卷聚合：?volume= 过滤（先 ACL，不在视图 → 404）。
	vols := volume.AllowedVolumes(s.rt.volSet().All(), owner)
	if q.VolName != "" {
		v, ok := s.rt.volSet().ByName(q.VolName)
		if !ok || !v.Authorize(owner) {
			return empty, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
		}
		vols = []volume.Volume{v}
	}
	if len(vols) == 0 {
		// owner 视图为空（全部卷 ACL 拒）：按空目录返回（不可见卷绝不列出，AD-2）。
		return empty, nil
	}

	// 子目录校验（与默认租户路径映射同一规则；功能桶天然不可枚举）。
	rel, ok := s.listRelForOwner(owner, subdir)
	if !ok {
		return ListResult{Files: []FileInfo{}}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}

	csMap := s.checksumSnapshot(owner)
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
	sortFileEntries(allFiles, q.SortBy, q.SortOrder)
	return ListResult{
		Files: paginateEntries(allFiles, q.Offset, q.Limit), Total: len(allFiles),
		Offset: q.Offset, Limit: q.Limit,
	}, nil
}

// Search 实现 GET /api/files/search 的领域逻辑：owner 可见卷内按文件名子串递归匹配。
//
// 错误语义：`*HTTPError{400}` = 搜索词为空 / owner 不可用 / 派生搜索根失败。
func (s *Service) Search(q SearchQuery) (ListResult, error) {
	query := strings.TrimSpace(q.Query)
	if query == "" {
		return ListResult{Files: []FileInfo{}}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	qLower := strings.ToLower(query)
	owner := normalizeOwner(q.Owner)
	// 一次性快照（见 ListFiles #8 结论注释，勿再分析）：per-tenant store，key 为相对租户根的 rel。
	csMap := s.checksumSnapshot(owner)

	if s.rt.volSet() == nil {
		// 旧装配路径：单卷唯一根（搜索根 = user 桶绝对路径；功能桶天然不参与搜索）。
		tnt := s.rt.tenantOf(owner)
		if tnt == nil || tnt.Root() == nil {
			return ListResult{Files: []FileInfo{}}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
		}
		searchRoot, ok := tnt.Root().Abs(tnt.UserRoot())
		if !ok {
			return ListResult{Files: []FileInfo{}}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
		}
		var results []FileInfo
		s.collectSearchResults(searchRoot, qLower, csMap, "", &results, make(map[string]bool))
		return ListResult{Files: results, Total: len(results), Offset: 0, Limit: len(results)}, nil
	}

	// 多卷：owner 视图逐卷搜索。
	vols := volume.AllowedVolumes(s.rt.volSet().All(), owner)
	if len(vols) == 0 {
		return ListResult{Files: []FileInfo{}, Total: 0, Offset: 0, Limit: 0}, nil
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
	return ListResult{Files: results, Total: len(results), Offset: 0, Limit: len(results)}, nil
}

// StatPath 对**已解析**的下载路径取元信息。
//
// `dp` 由装配层的 DownloadPaths 能力解析（含 kind 白名单 / 跨卷读定位 / 云任务归属校验）——
// 那属装配层策略，领域侧只消费结果（见包文档「只读面的分工」）。
//
// 错误语义：`*HTTPError{404}` 不存在；`*HTTPError{500}` 其他 IO 错。
func (s *Service) StatPath(dp DownloadPath) (FileStat, error) {
	info, err := dp.Tenant.Root().Stat(dp.Rel)
	if err != nil {
		if os.IsNotExist(err) {
			return FileStat{}, &HTTPError{Status: http.StatusNotFound, Message: "not found"}
		}
		s.rt.logger().Error("stat 失败", "file_name", dp.Filename, "error", err.Error())
		return FileStat{}, &HTTPError{Status: http.StatusInternalServerError, Message: "stat error"}
	}
	st := FileStat{IsDir: info.IsDir(), Size: info.Size(), MTime: info.ModTime().UnixNano()}
	if csStore, csKey := s.checksumStoreForRead(dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			st.Checksum = cs
		} else if !info.IsDir() {
			if cs, cerr := fileChecksumRoot(dp.Tenant.Root(), dp.Rel); cerr == nil {
				st.Checksum = cs
			}
		}
	}
	return st, nil
}

// OpenPath 打开**已解析**的下载路径供读取（调用方负责 Close）。
//
// 与 StatPath 同一分工：`dp` 来自装配层解析。Range/206 由 HTTP 层的 ServeContent 承担，
// 本方法只保证「打开 + 元信息 + checksum（台账命中或实时计算）」。
//
// 错误语义：`*HTTPError{404}` 不存在（errMsgFileNotFound）；`*HTTPError{500}` 打开或
// stat 失败（errMsgOpenFileFailed / "stat 失败"）。
func (s *Service) OpenPath(dp DownloadPath) (OpenedFile, error) {
	// 所有下载 kind 均经租户根打开（root 相对，防符号链接逃逸）。
	file, err := dp.Tenant.Root().Open(dp.Rel)
	if err != nil {
		if os.IsNotExist(err) {
			return OpenedFile{}, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
		}
		s.rt.logger().Error("打开文件失败", "file_name", dp.Filename, "error", err.Error())
		return OpenedFile{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgOpenFileFailed}
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		s.rt.logger().Error("stat 文件失败", "file_name", dp.Filename, "error", err.Error())
		return OpenedFile{}, &HTTPError{Status: http.StatusInternalServerError, Message: "stat 失败"}
	}

	out := OpenedFile{File: file, Info: info}
	// 设置 SHA-256 checksum：优先从 store 读取，回退实时计算。
	// 回退路径优先复用已打开的文件句柄（零额外 I/O），仅当计算成功后才写入缓存。
	// 统一 per-tenant store + 根内相对路径 key（无 owner 前缀，与写端 checksumStoreFor 一致）。
	if csStore, csKey := s.checksumStoreForRead(dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			out.Checksum = cs
		} else {
			_, _ = file.Seek(0, io.SeekStart)
			if cs, cerr := checksumReader(file); cerr == nil {
				_, _ = file.Seek(0, io.SeekStart)
				csStore.Set(csKey, cs)
				out.Checksum = cs
			} else {
				s.rt.logger().Warn("计算文件 checksum 失败", "error", cerr.Error(), "file_name", dp.Filename)
			}
		}
	}
	return out, nil
}

// checksumSnapshot 返回 owner 的 checksum 台账快照（未装配 → 空 map）。
//
// 抽成方法是为了让 List/Search 的"一次性快照"约定只有一处（见既有注释：per-tenant store，
// key 为相对租户根的 rel）。
func (s *Service) checksumSnapshot(owner string) map[string]string {
	if cs := s.rt.checksumStore(owner); cs != nil {
		return cs.GetAll()
	}
	return map[string]string{}
}

// asHTTPError 取出 *HTTPError（非该类型返回 nil）。
func asHTTPError(err error) *HTTPError {
	var he *HTTPError
	if errors.As(err, &he) {
		return he
	}
	return nil
}
