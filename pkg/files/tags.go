// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// tags.go 是文件标签系统（roadmap 11.10-④ / docs/designs/2026-09-24-file-tags.md）
// 的领域实现：
//
//   - tagsStore：per-owner 持久化（meta 桶 <owner>/meta/tags/<sha256(rel)>.json，
//     单文件原子写），是标签的**权威事实源**（内存索引只是缓存：索引丢失/失效重建时
//     从 store 重新合并，见 search_index.go 的 walkUserRoot / buildLocked）；
//   - TagFiles：写路径增量——store 原子写（成功才继续）→ index.setTags（COW 替换指针）；
//   - SearchQuery.Tag：search?tag= 精确匹配（与 q 的 base/contentTokens 匹配 AND 组合）。
//
// 校验规则：标签 `[A-Za-z0-9_\p{L}-]{1,32}`（1-32 字符），每文件 ≤20 个（去重后），
// 自动去重排序。文件不存在 → 404；标签非法/超量 → 400（**整批拒绝**，不部分成功）；
// store 写失败 → 500 且索引不更新（索引只是缓存，下次构建以 store 为准）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// errMsgTagsInvalid / errMsgTagsTooMany / errMsgTagsFileNotFound 是标签族的失败响应文案。
const (
	errMsgTagsInvalid      = "标签非法：仅允许字母/数字/下划线/中划线/Unicode 字母，长度 1-32"
	errMsgTagsTooMany      = "标签数量超限：每个文件最多 20 个标签"
	errMsgTagsFileNotFound = "文件不存在"
)

// maxTagsPerFile 是每个文件的最大标签数（设计文档：≤20）。
const maxTagsPerFile = 20

// tagPattern 是标签合法性正则（设计文档：`[A-Za-z0-9_\p{L}-]{1,32}`）。
// Go 的 regexp 用 `\pL`（无花括号）表 Unicode 字母段。
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_\pL-]{1,32}$`)

// TagsRequest 是 POST /api/tags 的 JSON body 批量形态（{files, tags}）。
type TagsRequest struct {
	Files []string `json:"files"`
	Tags  []string `json:"tags"`
}

// normalizeTags 校验并规范化标签列表：去重、排序；非法/超量返回错误。
// 入参可含重复（幂等）；空列表合法（清空标签）。
func normalizeTags(tags []string) ([]string, error) {
	if len(tags) > maxTagsPerFile {
		return nil, errors.New(errMsgTagsTooMany)
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue // 空串忽略（与去重同幂等语义）
		}
		if !tagPattern.MatchString(t) {
			return nil, errors.New(errMsgTagsInvalid)
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	slices.Sort(out)
	return out, nil
}

// tagsContain 判断标签列表是否精确含 tag（搜索 tag= 过滤用）。
func tagsContain(tags []string, tag string) bool {
	return slices.Contains(tags, tag)
}

// ---- tagsStore（权威事实源） ----

// tagStoreFile 是单文件 store 的 JSON 结构（未来可扩展，当前仅 tags）。
type tagStoreFile struct {
	Tags []string `json:"tags"`
}

// tagsStoreRel 返回某文件标签文件的租户根相对路径：meta/tags/<sha256(rel)>.json。
// rel 是相对 user 桶的路径（如 sub/f.txt）；sha256 十六进制（小写）作文件名，
// 避免 rel 含目录分隔符时展平冲突（设计文档：<owner>/meta/tags/<sha256(rel)>.json）。
func tagsStoreRel(rel string) string {
	sum := sha256.Sum256([]byte(filepath.ToSlash(rel)))
	return filepath.ToSlash(filepath.Join("meta", "tags", hex.EncodeToString(sum[:])+".json"))
}

// tagsStoreAbs 返回标签文件的绝对路径（租户不可用/派生失败 → 空）。
func tagsStoreAbs(t *storage.Tenant, rel string) string {
	if t == nil || t.Root() == nil {
		return ""
	}
	p, ok := t.Root().Abs(tagsStoreRel(rel))
	if !ok {
		return ""
	}
	return p
}

// loadTagsFromStore 读取某文件的标签（缺失/损坏 → nil，与「未打标」同语义）。
func loadTagsFromStore(t *storage.Tenant, rel string) []string {
	p := tagsStoreAbs(t, rel)
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var f tagStoreFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	if len(f.Tags) == 0 {
		return nil
	}
	slices.Sort(f.Tags)
	return f.Tags
}

// saveTagsToStore 原子写某文件的标签（tmp + rename；meta/tags 目录自动建）。
// 空标签 = 删除该标签文件（幂等；避免空文件残留）。
func saveTagsToStore(t *storage.Tenant, rel string, tags []string) error {
	p := tagsStoreAbs(t, rel)
	if p == "" {
		return errors.New("标签存储路径派生失败")
	}
	if len(tags) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(tagStoreFile{Tags: tags})
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp) // Windows 并发 rename 失败：清理 tmp 残留
		return err
	}
	return nil
}

// deleteTagsFromStore 删除某文件的标签文件（rmdir 子树清理 / rename 搬移旧键用）。
func deleteTagsFromStore(t *storage.Tenant, rel string) {
	p := tagsStoreAbs(t, rel)
	if p == "" {
		return
	}
	_ = os.Remove(p)
}

// tagsTenant 返回 owner 的标签权威租户（默认卷 = 唯一根；多卷时默认卷持有
// meta/tags——与 checksum 台账同一「默认卷权威」约定）。
func (s *Service) tagsTenant(owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	return s.rt.tenantOf(owner)
}

// TagFiles 打标/清空标签：store 原子写（成功才继续）→ index.setTags（COW 替换指针）。
//
// 错误语义（*HTTPError）：
//   - 400：标签非法 / 超量（**整批拒绝**，不部分成功）；
//   - 404：任一文件不存在（**整批拒绝**）；
//   - 500：tagsStore 写失败（索引不更新——索引只是缓存，下次构建以 store 为准）。
func (s *Service) TagFiles(owner string, rels, tags []string) error {
	owner = normalizeOwner(owner)
	normTags, err := normalizeTags(tags)
	if err != nil {
		return &HTTPError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	if len(rels) == 0 {
		return &HTTPError{Status: http.StatusBadRequest, Message: "文件列表不能为空"}
	}
	// 租户校验（owner 不可用 → 400）。
	tnt := s.tagsTenant(owner)
	if tnt == nil || tnt.Root() == nil {
		return &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	// 路径校验 + 存在性检查（整批拒绝：任一非法/不存在 → 整体失败，不部分成功）。
	// 存在性经卷定位（locateForRead 语义：owner 视图内任意卷命中即存在）。
	keys := make([]string, 0, len(rels))
	for _, rel := range rels {
		remotePath, vErr := pathguard.ValidateFilePath(rel)
		if vErr != nil {
			return &HTTPError{Status: http.StatusBadRequest, Message: vErr.Error()}
		}
		userRel, ok := tnt.UserRel(remotePath)
		if !ok {
			return &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
		}
		relKey := strings.TrimPrefix(userRel, tnt.UserRoot()+"/")
		if relKey == userRel {
			return &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
		}
		if !s.fileExistsForTag(owner, userRel) {
			return &HTTPError{Status: http.StatusNotFound, Message: errMsgTagsFileNotFound}
		}
		keys = append(keys, relKey)
	}
	// 批量写 store（每文件原子写）；任一失败 → 500（已写的不回滚：索引未更新，
	// 下次构建以 store 为准——与设计文档「索引只是缓存」一致）。
	for _, key := range keys {
		if err := saveTagsToStore(tnt, key, normTags); err != nil {
			return &HTTPError{Status: http.StatusInternalServerError, Message: "标签保存失败"}
		}
	}
	// 索引写路径增量（COW 替换指针）。索引未构建（nil 索引）时跳过——首次搜索会全量
	// 构建并从 store 合并。
	if s.index != nil {
		for _, key := range keys {
			s.index.setTags(owner, key, normTags)
		}
	}
	return nil
}

// fileExistsForTag 探测 owner 视图内 userRel 是否已存在（目录不算——打标对象是文件）。
// 经 locateForRead 全视图定位（AD-6 ACL 收口，默认卷被排除不得回落）。
func (s *Service) fileExistsForTag(owner, userRel string) bool {
	loc, found := s.locateForRead(owner, userRel, "")
	if !found || loc.Tenant == nil || loc.Tenant.Root() == nil {
		return false
	}
	info, err := loc.Tenant.Root().Stat(userRel)
	if err != nil || info.IsDir() {
		return false
	}
	return true
}

// Tags 处理 POST /api/tags。
//
// 两种形态（设计文档）：
//   - 查询参数：POST /api/tags?filename=<name>&tags=a,b,c（单个文件）；
//   - JSON body：{"files": [...], "tags": [...]}（批量，body 存在时优先）。
//
// 成功 → 200 {"success":true,"message":"标签已更新"}；失败按 *HTTPError 状态码回包。
func (s *Service) Tags(w http.ResponseWriter, r *http.Request) {
	owner := s.rt.actorOf(r)
	var rels, tags []string
	if r.Body != nil {
		var req TagsRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)) // 1MB
		if err := dec.Decode(&req); err == nil {
			// body 解析成功且 files 非空 → 批量形态。
			if len(req.Files) > 0 {
				rels = req.Files
				tags = req.Tags
			}
		} else if len(r.URL.Query().Get("filename")) == 0 {
			// body 解析失败且无查询参数兜底 → 400（body 非空但非法）。
			if err := drainAndVerifyBody(r); err == nil {
				s.sendJSON(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
				return
			}
		}
	}
	// 查询参数形态兜底（body 未提供批量或 body 为空）。
	if len(rels) == 0 {
		filename := r.URL.Query().Get("filename")
		if filename == "" {
			s.sendJSON(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
			return
		}
		rels = []string{filename}
		if raw := r.URL.Query().Get("tags"); raw != "" {
			tags = strings.Split(raw, ",")
		}
	}
	if err := s.TagFiles(owner, rels, tags); err != nil {
		if he := asHTTPError(err); he != nil {
			s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: "标签操作失败"}, http.StatusInternalServerError)
		return
	}
	s.sendJSON(w, UploadResponse{Success: true, Message: "标签已更新"}, http.StatusOK)
}
