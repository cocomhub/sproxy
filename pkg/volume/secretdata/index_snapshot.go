// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// indexSnapshotVersion 是索引快照格式版本（不兼容即作废并回退全量扫描）。
const indexSnapshotVersion = 1

// indexSnapshot 是装配期索引的**加密快照**：避免每次进程启动都重新下载并解密全部
// 文件 meta blob（外部卷上 272 个文件 ≈ 544 次 `BaiduPCS-Go download`，实测 20-30 分钟）。
//
// **与既有外部卷 cache 的对齐口径**（不新造缓存体系）：
//   - 落盘根与 volumestate 相同（`<storage_root>/meta/state/<卷>/`），路径由装配层经
//     extra.index_snapshot_path 注入（`volume_state.disable` 时不注入 ⇒ 零回归）；
//   - 生命周期与 `volumestate` 一致：进程内写由事件/写路径驱动，重启后仍可复用（本快照）
//     —— 唯一新增的是**加密**：保密卷也被允许持久化索引，因为落盘内容由卷密钥加密，
//     磁盘上不出现文件名/哈希明文（这是「仅增加加解密存储行为」的边界）；
//   - 不改变任何挂载/校验语义：命中后仍需容器集合一致 + 抽样 Stat 通过，否则全量重扫。
//
// 安全：整体 AES-256-GCM（key 由卷密钥派生），换密钥（指纹不符）即作废。
//
// 正确性：载入后仍需校验「容器集合一致 + 抽样 Stat 通过 + 密钥指纹/算法档位一致」，
// 任一不满足 → 丢弃快照并回退全量扫描（fail-safe，不降低「不完整视图」防护）。
type indexSnapshot struct {
	Version      int                        `json:"version"`
	Algorithm    string                     `json:"algorithm"`
	SecretFP     string                     `json:"secret_fp"`
	CreatedAt    string                     `json:"created_at"`
	Containers   []string                   `json:"containers"`
	Dirs         []string                   `json:"dirs"`
	DirSegs      map[string]string          `json:"dir_segs"`
	DirIDs       map[string]string          `json:"dir_ids"`
	DirPhysTotal int64                      `json:"dir_phys_total"`
	Entries      []indexSnapshotEntry       `json:"entries"`
	Extra        map[string]json.RawMessage `json:"extra,omitempty"`
}

// indexSnapshotEntry 是单文件索引项的序列化形态（metaEntry 的字段一一对应）。
type indexSnapshotEntry struct {
	Rel         string          `json:"rel"`
	Size        int64           `json:"size"`
	MTime       int64           `json:"mtime"`
	DirSeg      string          `json:"dir_seg"`
	MetaName    string          `json:"meta_name"`
	DataDir     string          `json:"data_dir,omitempty"`
	BaseVersion int64           `json:"base_version"`
	Phys        int64           `json:"phys"`
	Meta        *shardseal.Meta `json:"meta"`
}

// snapshotSecretFP 返回卷密钥指纹（用于快照校验，不落明文密钥）。
func snapshotSecretFP(secret []byte) string {
	sum := sha256.Sum256(append([]byte("secretdata-index-fp-v1"), secret...))
	return hex.EncodeToString(sum[:8])
}

// snapshotKey 派生快照加密 key（与内容加密域分离，避免复用同一密钥流）。
func snapshotKey(secret []byte) [32]byte {
	return sha256.Sum256(append([]byte("secretdata-index-enc-v1"), secret...))
}

// loadIndexSnapshot 读取并解密快照（失败一律返回 ok=false，由调用方回退全量扫描）。
func (s *SecretdataFS) loadIndexSnapshot() (*indexSnapshot, bool) {
	p := s.opts.IndexSnapshotPath
	if p == "" {
		return nil, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("secretdata: 读取索引快照失败（回退全量扫描）", "path", p, "error", err)
		}
		return nil, false
	}
	key := snapshotKey(s.secret)
	block, berr := aes.NewCipher(key[:])
	if berr != nil {
		return nil, false
	}
	gcm, gerr := cipher.NewGCM(block)
	if gerr != nil || len(raw) <= gcm.NonceSize() {
		return nil, false
	}
	plain, derr := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if derr != nil {
		slog.Warn("secretdata: 索引快照解密失败（密钥变化/文件损坏，回退全量扫描）", "path", p)
		return nil, false
	}
	var snap indexSnapshot
	if jerr := json.Unmarshal(plain, &snap); jerr != nil {
		slog.Warn("secretdata: 索引快照解析失败（回退全量扫描）", "path", p, "error", jerr)
		return nil, false
	}
	if snap.Version != indexSnapshotVersion || snap.SecretFP != snapshotSecretFP(s.secret) ||
		snap.Algorithm != s.opts.Algorithm {
		slog.Info("secretdata: 索引快照不适用（版本/密钥/算法变化，回退全量扫描）",
			"version", snap.Version, "algorithm", snap.Algorithm)
		return nil, false
	}
	return &snap, true
}

// snapshotContainersMatch 校验快照容器集合与本次列目录结果一致（外部直改盘的判据）。
func snapshotContainersMatch(snap *indexSnapshot, containers []string) bool {
	if len(snap.Containers) != len(containers) {
		return false
	}
	have := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		have[c] = struct{}{}
	}
	for _, c := range snap.Containers {
		if _, ok := have[c]; !ok {
			return false
		}
	}
	return true
}

// verifySnapshotSample 抽样 Stat 快照条目（默认 16 个）：任一缺失/size 不符即视为快照陈旧。
// 抽样成本 O(16) 次 Stat，远低于全量 544 次 meta blob 下载。
func (s *SecretdataFS) verifySnapshotSample(ctx context.Context, snap *indexSnapshot) error {
	const sampleSize = 16
	if len(snap.Entries) == 0 {
		return nil
	}
	step := max(len(snap.Entries)/sampleSize, 1)
	checked := 0
	for i := 0; i < len(snap.Entries) && checked < sampleSize; i += step {
		e := snap.Entries[i]
		checked++
		key := e.DirSeg + "/" + e.MetaName
		ent, serr := s.inner.Stat(ctx, key)
		if serr != nil {
			if errors.Is(serr, fs.ErrNotExist) || errors.Is(serr, os.ErrNotExist) {
				return fmt.Errorf("抽样条目缺失: %s", key)
			}
			return fmt.Errorf("抽样 Stat 失败 %s: %w", key, serr)
		}
		if ent == nil {
			return fmt.Errorf("抽样条目不存在: %s", key)
		}
	}
	return nil
}

// applyIndexSnapshot 把快照写回内存索引（等价于全量扫描后的状态）。
func (s *SecretdataFS) applyIndexSnapshot(snap *indexSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index = make(map[string]*metaEntry, len(snap.Entries))
	for _, e := range snap.Entries {
		s.index[e.Rel] = &metaEntry{
			size: e.Size, mtime: e.MTime, dirSeg: e.DirSeg, metaName: e.MetaName,
			meta: e.Meta, dataDir: e.DataDir, baseVersion: e.BaseVersion, phys: e.Phys,
		}
	}
	s.dirs = make(map[string]struct{}, len(snap.Dirs))
	for _, d := range snap.Dirs {
		s.dirs[d] = struct{}{}
	}
	s.dirSegs = make(map[string]string, len(snap.DirSegs))
	maps.Copy(s.dirSegs, snap.DirSegs)
	s.dirIDs = make(map[string]string, len(snap.DirIDs))
	maps.Copy(s.dirIDs, snap.DirIDs)
	s.dirPhysTotal = snap.DirPhysTotal
}

// resetIndex 丢弃已载入的快照索引（回退全量扫描前调用）。
func (s *SecretdataFS) resetIndex() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index = map[string]*metaEntry{}
	s.dirs = map[string]struct{}{}
	s.dirSegs = map[string]string{}
	s.dirIDs = map[string]string{}
	s.dirPhysTotal = 0
}

// buildIndexSnapshot 从当前内存索引构造快照（调用时机：全量扫描完成后）。
func (s *SecretdataFS) buildIndexSnapshot(containers []string) *indexSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := &indexSnapshot{
		Version: indexSnapshotVersion, Algorithm: s.opts.Algorithm,
		SecretFP:     snapshotSecretFP(s.secret),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		Containers:   append([]string(nil), containers...),
		DirPhysTotal: s.dirPhysTotal,
		DirSegs:      make(map[string]string, len(s.dirSegs)),
		DirIDs:       make(map[string]string, len(s.dirIDs)),
		Dirs:         make([]string, 0, len(s.dirs)),
		Entries:      make([]indexSnapshotEntry, 0, len(s.index)),
	}
	for d := range s.dirs {
		snap.Dirs = append(snap.Dirs, d)
	}
	maps.Copy(snap.DirSegs, s.dirSegs)
	maps.Copy(snap.DirIDs, s.dirIDs)
	for rel, e := range s.index {
		snap.Entries = append(snap.Entries, indexSnapshotEntry{
			Rel: rel, Size: e.size, MTime: e.mtime, DirSeg: e.dirSeg, MetaName: e.metaName,
			DataDir: e.dataDir, BaseVersion: e.baseVersion, Phys: e.phys, Meta: e.meta,
		})
	}
	return snap
}

// writeIndexSnapshot 加密落盘（失败仅告警：快照是性能优化，不阻断挂载）。
func (s *SecretdataFS) writeIndexSnapshot(snap *indexSnapshot) {
	p := s.opts.IndexSnapshotPath
	if p == "" {
		return
	}
	plain, jerr := json.Marshal(snap)
	if jerr != nil {
		slog.Warn("secretdata: 索引快照序列化失败（忽略）", "error", jerr)
		return
	}
	key := snapshotKey(s.secret)
	block, berr := aes.NewCipher(key[:])
	if berr != nil {
		return
	}
	gcm, gerr := cipher.NewGCM(block)
	if gerr != nil {
		return
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, rerr := rand.Read(nonce); rerr != nil {
		return
	}
	out := gcm.Seal(nonce, nonce, plain, nil)
	if merr := os.MkdirAll(filepath.Dir(p), 0o700); merr != nil {
		slog.Warn("secretdata: 索引快照目录创建失败（忽略）", "path", p, "error", merr)
		return
	}
	tmp := p + ".tmp"
	if werr := os.WriteFile(tmp, out, 0o600); werr != nil {
		slog.Warn("secretdata: 索引快照写入失败（忽略）", "path", p, "error", werr)
		return
	}
	if rerr := os.Rename(tmp, p); rerr != nil {
		_ = os.Remove(tmp)
		slog.Warn("secretdata: 索引快照落盘失败（忽略）", "path", p, "error", rerr)
		return
	}
	slog.Info("secretdata: 索引快照已更新", "path", p, "entries", len(snap.Entries), "containers", len(snap.Containers))
}
