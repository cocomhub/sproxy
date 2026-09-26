// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// vector_index.go 是向量索引（roadmap 11.9-④ 语义搜索）：
//   - VectorStore：按 owner 分片的内存 map（rel → vectorEntry）+ gob 落盘快照；
//   - 写路径同步登记占位（rev/mtime），向量由外部（事件 worker）补齐；
//   - 删除/重命名同步更新（同 searchIndex map 操作模式）。
//
// 落盘 <缓存根>/<owner>.bin（gob）；损坏 → 忽略（幂等，下次 SaveAll 覆盖）。

import (
	"encoding/gob"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// vectorEntry 是 per-rel 向量 + 元数据（幂等：rev 未变不重 embedding）。
type vectorEntry struct {
	Vec  []float32 // 归一化 embedding（空 = 占位未补齐）
	Rev  int64     // 来源文件 rev/mtime（幂等去重）
	Text string    // 已 embedding 的文本摘录（溯源/重建）
}

// VectorStore 是按 owner 分片的向量存储。
type VectorStore struct {
	mu      sync.RWMutex
	byOwner map[string]map[string]*vectorEntry
	dir     string // 落盘根目录（空 = 不落盘）
}

// NewVectorStore 构造向量存储（装配层导出；dir 为空 = 纯内存）。
func NewVectorStore(dir string) *VectorStore {
	return &VectorStore{byOwner: map[string]map[string]*vectorEntry{}, dir: dir}
}

// Put 登记/覆盖一个文件条目（幂等：同 owner+rel 覆盖）。
func (vs *VectorStore) Put(owner, rel string, vec []float32, rev int64, text string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.byOwner[owner] == nil {
		vs.byOwner[owner] = map[string]*vectorEntry{}
	}
	vs.byOwner[owner][rel] = &vectorEntry{Vec: vec, Rev: rev, Text: text}
}

// Get 读取条目（不存在 → nil）。
func (vs *VectorStore) Get(owner, rel string) *vectorEntry {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.byOwner[owner][rel]
}

// Delete 删除条目。
func (vs *VectorStore) Delete(owner, rel string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if m := vs.byOwner[owner]; m != nil {
		delete(m, rel)
	}
}

// Rename 同步重命名（from → to）。
func (vs *VectorStore) Rename(owner, from, to string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	m := vs.byOwner[owner]
	if m == nil {
		return
	}
	if e, ok := m[from]; ok {
		delete(m, from)
		m[to] = e
	}
}

// Search 余弦 top-k（含占位条目返回零分；q 向量由调用方提供）。
// 返回降序 (rel, score, entry)；k <= 0 或超过条目数 → 全部。
func (vs *VectorStore) Search(owner string, q []float32, k int) []VectorHit {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	m := vs.byOwner[owner]
	if m == nil {
		return nil
	}
	all := make([]VectorHit, 0, len(m))
	for rel, e := range m {
		if len(e.Vec) == 0 {
			continue // 占位（向量未补齐）不参与语义搜索
		}
		all = append(all, VectorHit{Rel: rel, Score: cosine(q, e.Vec), Entry: e})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if k > 0 && len(all) > k {
		all = all[:k]
	}
	return all
}

// VectorHit 是搜索命中（rel + 余弦分数 + 条目）。
type VectorHit struct {
	Rel   string
	Score float64
	Entry *vectorEntry
}

// SaveAll 全量落盘（每 owner 一个 <dir>/<owner>.bin；gob）。
func (vs *VectorStore) SaveAll() error {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	if vs.dir == "" {
		return nil
	}
	if err := os.MkdirAll(vs.dir, 0o755); err != nil {
		return err
	}
	for owner, m := range vs.byOwner {
		path := filepath.Join(vs.dir, owner+".bin")
		f, err := os.CreateTemp(vs.dir, owner+"-*.tmp")
		if err != nil {
			return err
		}
		tmp := f.Name()
		if err := gob.NewEncoder(f).Encode(m); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}

// LoadAll 从落盘载入全部 owner 快照（损坏 → 忽略该文件）。
func (vs *VectorStore) LoadAll() {
	if vs.dir == "" {
		return
	}
	entries, err := os.ReadDir(vs.dir)
	if err != nil {
		return
	}
	for _, de := range entries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".bin" {
			continue
		}
		owner := de.Name()[:len(de.Name())-4]
		path := filepath.Join(vs.dir, de.Name())
		m := map[string]*vectorEntry{}
		if err := func() error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			return gob.NewDecoder(f).Decode(&m)
		}(); err != nil {
			continue // 损坏 → 忽略（幂等）
		}
		vs.mu.Lock()
		vs.byOwner[owner] = m
		vs.mu.Unlock()
	}
}

// cosine 余弦相似度（两向量已归一化 → 点积即余弦）。
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

var _ = math.Abs

// SetVectorStore 注入向量索引（装配层调用；nil = 未装配，语义端点 404）。
func (s *Service) SetVectorStore(vs *VectorStore) { s.vector = vs }

// SemanticSearch 语义搜索：q → Embed → 余弦 top-k → FileInfo 合并。
// 未装配向量索引 → ErrNoVector（端点 404）。
// Embed 失败/无向量 → 空结果（调用方决定关键词回退）。
func (s *Service) SemanticSearch(q SearchQuery, vec []float32, topk int) (SemanticResult, error) {
	if s.vector == nil {
		return SemanticResult{}, errNoVector
	}
	if strings.TrimSpace(q.Query) == "" {
		return SemanticResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	owner := normalizeOwner(q.Owner)
	csMap := s.checksumSnapshot(owner)
	hits := s.vector.Search(owner, vec, topk)
	results := make([]SemanticHit, 0, len(hits))
	for _, h := range hits {
		fi := FileInfo{Name: h.Rel, ModTime: h.Entry.Rev}
		if cs, ok := csMap["user/"+h.Rel]; ok {
			fi.Checksum = cs
		}
		results = append(results, SemanticHit{File: fi, Score: h.Score})
	}
	return SemanticResult{Results: results, Mode: "semantic"}, nil
}

// SemanticResult 是语义搜索响应。
type SemanticResult struct {
	Results []SemanticHit `json:"results"`
	Mode    string        `json:"mode"` // "semantic" | "keyword"（回退）
}

// SemanticHit 是单条命中。
type SemanticHit struct {
	File  FileInfo `json:"file"`
	Score float64  `json:"score"`
}

// errNoVector 是向量索引未装配的哨兵错误。
var errNoVector = errors.New("semantic search not configured")

// SemanticSearchHTTP 是 GET /api/search/semantic 处理器：
//   - 未装配向量索引 → 404（零回归）；
//   - q 空 → 400；
//   - 装配但语义不可用（占位无向量）→ 回退关键词模式（mode=keyword，fail-safe 不 500）；
//   - 语义命中 → mode=semantic。
func (s *Service) SemanticSearchHTTP(w http.ResponseWriter, r *http.Request) {
	if s.vector == nil {
		s.sendJSON(w, map[string]any{"error": "semantic search not configured"}, http.StatusNotFound)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.sendJSON(w, map[string]any{"error": errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	owner := ""
	if s.rt.actor != nil {
		owner = s.rt.actorOf(r)
	}
	if o, ok := r.Context().Value(semanticOwnerKey{}).(string); ok && o != "" {
		owner = o
	}
	// 语义路径：q 已由装配层向量化后经 ctx 注入（语义端点由装配层包装 Embed(q)）。
	// 未注入向量 → 关键词回退（mode=keyword 可观测）。
	if vec := s.vectorQueryVector(r); vec != nil {
		res, err := s.SemanticSearch(SearchQuery{Owner: owner, Query: q}, vec, s.vectorTopK(r))
		if err != nil {
			s.sendJSON(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		s.sendJSON(w, res, http.StatusOK)
		return
	}
	res := s.keywordFallback(r, q, owner)
	s.sendJSON(w, res, http.StatusOK)
}

// vectorQueryVector 从请求取已向量化的 q（装配层经 ctx 注入；未注入 nil → 回退）。
func (s *Service) vectorQueryVector(r *http.Request) []float32 {
	if v, ok := r.Context().Value(semanticQueryVecKey{}).([]float32); ok {
		return v
	}
	return nil
}

// vectorTopK 解析 topk 参数（默认 10，上限 50）。
func (s *Service) vectorTopK(r *http.Request) int {
	k := 10
	if v := r.URL.Query().Get("topk"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			k = n
		}
	}
	if k > 50 {
		k = 50
	}
	return k
}

// keywordFallback 回退关键词检索（mode=keyword）。
// 零值 Service（未装配租户解析）→ 空结果（不 panic；生产路径恒装配）。
func (s *Service) keywordFallback(r *http.Request, q, owner string) SemanticResult {
	if s.rt.tenants == nil {
		return SemanticResult{Results: nil, Mode: "keyword"}
	}
	res, err := s.Search(SearchQuery{Owner: owner, Query: q})
	if err != nil {
		return SemanticResult{Results: nil, Mode: "keyword"}
	}
	out := make([]SemanticHit, 0, len(res.Files))
	for _, fi := range res.Files {
		out = append(out, SemanticHit{File: fi})
	}
	return SemanticResult{Results: out, Mode: "keyword"}
}

// semanticQueryVecKey 是 ctx 键（装配层注入已向量化的 q）。
type semanticQueryVecKey struct{}

// semanticOwnerKey 是 ctx 键（测试/装配层注入显式 owner；生产走 actorOf）。
type semanticOwnerKey struct{}
