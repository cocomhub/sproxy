// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// indexSnapshotVersion 是索引快照格式版本（不兼容即作废并回退全量扫描）。
const indexSnapshotVersion = 1

// snapshotGzipMagic 标记 gzip 压缩的快照（旧格式无该前缀，向后兼容）。
const snapshotGzipMagic = "SDIS2"

// indexSnapshot 是装配期索引的**加密快照**：避免每次进程启动都重新下载并解密全部
// 文件 meta blob（外部卷上 272 个文件 ≈ 544 次 `BaiduPCS-Go download`，实测 20-30 分钟）。
//
// **与既有外部卷 cache 的对齐口径**（不新造缓存体系）：
//   - 落盘根与 volumestate 相同（`<storage_root>/state/<卷>/`），路径由装配层经
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

	// isGzip 标记本次载入的来源是否为压缩格式（不序列化）：旧格式命中后后台重写升级。
	isGzip bool
}

// indexSnapshotEntry 是单文件索引项的序列化形态（metaEntry 的字段一一对应）。
type indexSnapshotEntry struct {
	Rel         string `json:"rel"`
	Size        int64  `json:"size"`
	MTime       int64  `json:"mtime"`
	DirSeg      string `json:"dir_seg"`
	MetaName    string `json:"meta_name"`
	DataDir     string `json:"data_dir,omitempty"`
	BaseVersion int64  `json:"base_version"`
	Phys        int64  `json:"phys"`
	// OrigSHA256 是**热字段**（从 `meta.Original.SHA256` 提升）：`Stat` 要自报整文件
	// checksum，而它**不应**为此把整个 Meta（含 blocklets）读进内存。
	//
	// ⚠️ **快照不得承载 `Meta`**（P1，2026-11-12，OOM 根因修复）：实测单个大文件的
	// meta 常驻内存达 78 MB（430,652 个 blocklets × 191 B），而热路径（ListDir/Stat/搜索）
	// **完全不用**它 —— 只有 openRead/OpenRangeRead 需要 ⇒ 改为由 ensureEntryMeta 惰性加载。
	// 详见 docs/designs/2026-11-12-secretdata-index-lazy-meta-and-cache-layers.md。
	OrigSHA256 string `json:"orig_sha256,omitempty"`
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
//
// **带进程内缓存**（P0 内存优化，2026-11-12）：快照 241.8 MB，解密+反序列化占 inuse_space
// 的 56.9%（pprof 实测），而两条快速路径各自调用本函数 ⇒ 一文一读会重复展开两次。
// 缓存后同进程只展开一次；失败也缓存（避免反复读 242MB 磁盘 + 解密失败）。
func (s *SecretdataFS) loadIndexSnapshot() (*indexSnapshot, bool) {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if s.snapDone {
		return s.snapCache, s.snapOK
	}
	snap, ok := s.loadIndexSnapshotUncached()
	s.snapCache, s.snapOK, s.snapDone = snap, ok, true
	return snap, ok
}

// loadIndexSnapshotUncached 是未缓存的真实实现（仅由 loadIndexSnapshot 调用）。
func (s *SecretdataFS) loadIndexSnapshotUncached() (*indexSnapshot, bool) {
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
	if gerr != nil {
		return nil, false
	}
	// 新格式带 gzip magic 前缀；旧格式（无 magic）按明文 JSON 直接解析（向后兼容）。
	isGzip := len(raw) > len(snapshotGzipMagic) && string(raw[:len(snapshotGzipMagic)]) == snapshotGzipMagic
	if isGzip {
		raw = raw[len(snapshotGzipMagic):]
	}
	if len(raw) <= gcm.NonceSize() {
		return nil, false
	}
	// **原地解密**（P0 内存优化，2026-11-12）：以 `raw[:0]` 作为 dst 让 GCM 把明文写回**密文所在的同一底层数组**
	// （crypto/cipher 文档明确支持 `ciphertext[:0]` 作为 dst）。
	// 为什么值得：快照实测 241.8 MB，原实现 `gcm.Open(nil, ...)` 会在密文之外**再分配一份等长明文**
	// （即 242MB 常驻共存，pprof 里体现为 io.ReadAll 473MB 的一项）；原地解密后密文被明文覆盖，
	// 峰值直接省下 242 MB。nonce 会被覆盖，但它在解密前已作为参数取走，无需保留。
	// dst 必须是 **ciphertext[:0]**（即 `raw[nonceSize:][:0]`），不是 `raw[:0]`：
	// `ciphertext[:0]` 指向密文起点；若用 `raw[:0]` 会偏移 nonce 长度，导致重叠写错位（实测测试红）。
	ct := raw[gcm.NonceSize():]
	plain, derr := gcm.Open(ct[:0], raw[:gcm.NonceSize()], ct, nil)
	if derr != nil {
		slog.Warn("secretdata: 索引快照解密失败（密钥变化/文件损坏，回退全量扫描）", "path", p)
		return nil, false
	}
	raw = nil // 密文已被明文覆盖（同一底层数组）⇒ 及时断开引用，便于 GC 识别（P0 内存优化）。
	var snap indexSnapshot
	if !decodeSnapshotInto(plain, isGzip, &snap, maxSnapshotDecodedBytes) {
		slog.Warn("secretdata: 索引快照解析失败（回退全量扫描）", "path", p)
		return nil, false
	}
	if snap.Version != indexSnapshotVersion || snap.SecretFP != snapshotSecretFP(s.secret) ||
		snap.Algorithm != s.opts.Algorithm {
		slog.Info("secretdata: 索引快照不适用（版本/密钥/算法变化，回退全量扫描）",
			"version", snap.Version, "algorithm", snap.Algorithm)
		return nil, false
	}
	snap.isGzip = isGzip
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
	// 抽样 8 条、4 并发：只做健全性检查（全部列举已被快照容器集合替代），
	// 外部卷每次 Stat 是 CLI 调用，串行 16 次要几秒 ⇒ 并发后约 1-2 秒。
	const sampleSize, sampleParallel = 8, 4
	if len(snap.Entries) == 0 {
		return nil
	}
	step := max(len(snap.Entries)/sampleSize, 1)
	keys := make([]string, 0, sampleSize)
	for i := 0; i < len(snap.Entries) && len(keys) < sampleSize; i += step {
		keys = append(keys, snap.Entries[i].DirSeg+"/"+snap.Entries[i].MetaName)
	}
	sem := make(chan struct{}, sampleParallel)
	errs := make([]error, len(keys))
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ent, serr := s.inner.Stat(ctx, key)
			switch {
			case serr != nil && (errors.Is(serr, fs.ErrNotExist) || errors.Is(serr, os.ErrNotExist)):
				errs[i] = fmt.Errorf("抽样条目缺失: %s", key)
			case serr != nil:
				errs[i] = fmt.Errorf("抽样 Stat 失败 %s: %w", key, serr)
			case ent == nil:
				errs[i] = fmt.Errorf("抽样条目不存在: %s", key)
			}
		}(i, key)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
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
			// **meta 留空**（P1）：由 ensureEntryMeta 在真正需要时（openRead/OpenRangeRead）
			// 从 metacache/meta blob 惰性加载 ⇒ 快照不再把冷结构带进内存。
			origSHA256: e.OrigSHA256,
			dataDir:    e.DataDir, baseVersion: e.BaseVersion, phys: e.Phys,
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
			DataDir: e.dataDir, BaseVersion: e.baseVersion, Phys: e.phys,
			// 热字段优先取扁平值；meta 恰好已在内存时顺便刷新（保持两者一致）。
			OrigSHA256: e.hotOrigSHA256(),
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
	// **本进程已发生写/迁移 ⇒ 放弃落盘**（2026-11-07 实测竞态）：装配期快照写入是**异步**
	// goroutine，而「容器名规范化」等写动作在其后**同步**执行并删快照 ⇒ 异步写入随后又把
	// **迁移前**的 dirSegs 写回 ⇒ 下次启动载入陈旧名字（实测：采样校验 8/8 全漂移）。
	if s.snapshotWriteDisabled.Load() {
		slog.Debug("secretdata: 索引快照落盘已抑制（本进程发生过写操作）")
		return
	}
	gen := s.snapshotGen.Load() // 代际快照：提交前比对（见 finalizeSnapshotFile）
	raw, jerr := json.Marshal(snap)
	if jerr != nil {
		slog.Warn("secretdata: 索引快照序列化失败（忽略）", "error", jerr)
		return
	}
	// gzip 压缩：chunks 列表（hex 字符串）重复度极高，401MB → 约 20-40MB，
	// 显著降低磁盘占用与 GCM 解密的字节量。
	var zbuf bytes.Buffer
	zw := gzip.NewWriter(&zbuf)
	if _, werr := zw.Write(raw); werr != nil {
		_ = zw.Close()
		return
	}
	if cerr := zw.Close(); cerr != nil {
		return
	}
	plain := zbuf.Bytes()
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
	out = append([]byte(snapshotGzipMagic), out...)
	if merr := os.MkdirAll(filepath.Dir(p), 0o700); merr != nil {
		slog.Warn("secretdata: 索引快照目录创建失败（忽略）", "path", p, "error", merr)
		return
	}
	tmp := p + ".tmp"
	if werr := os.WriteFile(tmp, out, 0o600); werr != nil {
		slog.Warn("secretdata: 索引快照写入失败（忽略）", "path", p, "error", werr)
		return
	}
	if !s.finalizeSnapshotFile(tmp, p, gen) {
		return // 已放弃提交（窗口内发生过写操作 / rename 失败）
	}
	slog.Info("secretdata: 索引快照已更新", "path", p, "entries", len(snap.Entries), "containers", len(snap.Containers))
}

// finalizeSnapshotFile 提交快照文件（tmp → p），**提交前重新校验代际**：
// 调用方在进入落盘流程时捕获 gen，此处若发现 `s.snapshotGen` 已变化（窗口内发生了写操作并删除了
// 快照）则放弃提交并清理 tmp。
//
// **为什么必须在 rename 前再查一次**（批次 16 P1-2）：入口处的 `snapshotSuppressed` 检查与
// `os.Rename` 之间隔着 marshal + gzip + AES-GCM + 写盘（数十 MB、秒级窗口）——窗口内落下的
// 写/迁移无法阻止陈旧快照被写回，下次启动即载入陈旧索引（已删文件“复活”、新文件“不可见”）。
func (s *SecretdataFS) finalizeSnapshotFile(tmp, p string, gen uint64) bool {
	if cur := s.snapshotGen.Load(); cur != gen {
		_ = os.Remove(tmp)
		slog.Warn("secretdata: 索引快照提交前发现代际已变化（窗口内发生过写操作）⇒ 放弃落盘，避免陈旧索引覆盖",
			"path", p, "gen", gen, "current_gen", cur)
		return false
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		slog.Warn("secretdata: 索引快照落盘失败（忽略）", "path", p, "error", err)
		return false
	}
	return true
}

// invalidateIndexSnapshot 使索引快照失效（**写路径必须调用**）。
//
// 为什么必须：快照只反映「装配时刻」的索引。任何写操作（写入/覆盖/删除/改名/建删目录）
// 之后若保留快照，下次启动会载入陈旧索引 ⇒ 已删文件「复活」、新文件「不可见」。
// 成本 O(1)（删一个文件）；下次启动全量重建，但因 meta blob 已在本机密文缓存
// （Options.MetaCacheDir），重建只需本地读 + 解密 + 容器列举，不再触发网盘全量下载。
func (s *SecretdataFS) invalidateIndexSnapshot() {
	if s.opts.IndexSnapshotPath == "" {
		return
	}
	s.snapshotWriteDisabled.Store(true) // 抑制装配期**异步**落盘（进程级闩锁，见字段注释）
	s.snapshotGen.Add(1)                // 代际自增 ⇒ 在途的慢路径写入提交前会放弃
	if err := os.Remove(s.opts.IndexSnapshotPath); err == nil {
		slog.Debug("secretdata: 索引快照已失效（写操作后需重建）")
	}
}

// snapshotRootMatches 校验快照容器树的**顶层目录集合**与本次卷根列举一致。
// 「跳过容器树列举」时用它做健全性检查（成本 1 次 ListDir）：顶层增删可发现；
// 深层变化由抽样 Stat + 「本进程写即删快照」覆盖。
func snapshotRootMatches(snap *indexSnapshot, root []syncpkg.Entry) bool {
	want := topSegments(snap.Containers)
	have := make(map[string]struct{}, len(root))
	for _, e := range root {
		if e.IsDir {
			have[e.Name] = struct{}{}
		}
	}
	if len(want) != len(have) {
		return false
	}
	for k := range want {
		if _, ok := have[k]; !ok {
			return false
		}
	}
	return true
}

// topSegments 返回容器路径的第一段集合（去重）。
func topSegments(containers []string) map[string]struct{} {
	out := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		if i := strings.IndexByte(c, '/'); i > 0 {
			out[c[:i]] = struct{}{}
		} else if c != "" {
			out[c] = struct{}{}
		}
	}
	return out
}

// tryLoadIndexSnapshotFast 在**不列举整棵容器树**的前提下尝试恢复索引：
// 快照可解密 + 卷根顶层目录集合一致 + 抽样 Stat 通过 ⇒ 直接应用。
// 这把外部卷启动从「292 次 ls（~3 分钟）」降到「1 次 ls + 16 次 Stat（秒级）」。
func (s *SecretdataFS) tryLoadIndexSnapshotFast(ctx context.Context, root []syncpkg.Entry) bool {
	snap, ok := s.loadIndexSnapshot()
	if !ok {
		return false
	}
	if !snapshotRootMatches(snap, root) {
		slog.Info("secretdata: 快照顶层目录集合已变化（回退全量扫描）")
		return false
	}
	if verr := s.verifySnapshotSample(ctx, snap); verr != nil {
		slog.Warn("secretdata: 索引快照抽样校验失败（回退全量扫描）", "error", verr)
		return false
	}
	s.applyIndexSnapshot(snap)
	slog.Info("secretdata: 索引快照命中（跳过全量 meta 扫描与容器列举）",
		"entries", len(snap.Entries), "containers", len(snap.Containers))
	// 旧格式（未压缩）命中后后台重写为 gzip 格式：下次启动解密量降一个数量级。
	if !snap.isGzip {
		containers := append([]string(nil), snap.Containers...)
		go func() {
			if s.snapshotWriteDisabled.Load() {
				return
			}
			s.writeIndexSnapshot(s.buildIndexSnapshot(containers))
		}()
	}
	return true
}

// maxSnapshotDecodedBytes 是快照解压后的上限（防解压炸弹；正常快照 401MB 级，留 8× 余量）。
const maxSnapshotDecodedBytes = 3 << 30

// countingReader 统计下游实际消费的字节数（防解压炸弹判据：流式下没有物化 buffer 可量长度）。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// decodeSnapshotInto 把快照明文解码进 snap（gzip 格式**直连解码器**）。
// 任何异常 ⇒ ok=false（调用方回退全量扫描）。
//
// **P0 内存优化（2026-11-12，#2）**：不再把解压结果物化成一份完整 buffer。
// 快照实测 241.8 MB（gzip），解压后 ~473 MB —— pprof 里 `io.ReadAll` 473MB 就是它；
// 这份 buffer 与解码峰值叠加，是启动期内存峰值的主要成分（曾致 OOM）。
//
// 实测（30 万目录合成夹具，HeapInuse 峰值，200µs 采样；绝对值为单次量级，看**相对关系**）：
//
//	现状：解压物化 + v1 Unmarshal             300–400 MB  基准
//	手写 jsontext token 流式（逐 token）       ~460 MB     比现状**更差**
//	PeekKind + 逐条 UnmarshalDecode            ~280 MB     仍差
//	PeekKind + 逐条 + 预分配容量               ~250 MB     仍差
//	本方案：gzip 直连 + v2 UnmarshalDecode     ~130–170 MB **最低（约为现状的 1/3 左右）**
//
// ⚠️ 不要回头走这两条路（均已实测否决）：
//  1. **手写 token 状态机**（`ReadToken` 逐 token 手工填 map）：比现状更费 ——
//     每个字符串都要 `strings.Clone`，`Token` 会被后续 `ReadToken`/`ReadValue` 调用
//     作废（实测 panic `invalid jsontext.Token; it has been voided by a subsequent
//     json.Decoder call`），逐条 append 又触发 map 几何扩容。
//  2. **`PeekKind` + 逐条 `UnmarshalDecode`**（心智成本最低的写法，`dec.PeekKind() != ']'`
//     循环内一次解一个值）：当**必须保留全部条目**时它仍劣于整体解码（+81 MB，即使预分配）
//     —— v2 整体解码对 slice/map 的分配策略更优。
//     ⇒ 该写法只适合「**逐条消费、不留存**」的场景（此时内存 O(1)）；本场景要留存全量索引。
//     附：`SkipValue` 跳过不需要的字段对内存几乎无影响（±2 MB），但省构造/CPU，可用于明确
//     不需要的字段。
//
// 收益的真正来源是「**不物化解压 buffer**」，不是「逐 token / 逐条」。
//
// 兼容性：非 gzip（旧格式）路径保留，语义与原先一致；gzip 路径的
// 字段名匹配从 v1 的「忽略大小写」变为 v2 的「精确匹配」—— 快照由本包 `json.Marshal`
// 写出（字段名精确），故不影响；异常输入一律 fail-closed 回退全量扫描。
func decodeSnapshotInto(plain []byte, isGzip bool, snap *indexSnapshot, maxBytes int64) bool {
	if !isGzip {
		// 旧格式无压缩体可省，仍需防「巨大明文 JSON」：按输入长度预判。
		if int64(len(plain)) >= maxBytes {
			return false
		}
		return jsonv2.Unmarshal(plain, snap) == nil
	}
	zr, zerr := gzip.NewReader(bytes.NewReader(plain))
	if zerr != nil {
		return false
	}
	defer func() { _ = zr.Close() }()
	cr := &countingReader{r: zr}
	if err := jsonv2.UnmarshalDecode(jsontext.NewDecoder(cr), snap); err != nil {
		return false
	}
	// 防解压炸弹：解码**实际消费**的字节数超上限即拒绝（原先对物化 buffer 长度做同样限制）。
	return cr.n < maxBytes
}
