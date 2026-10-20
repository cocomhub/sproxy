// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// journal.go 是审计条目的**持久化后端**（append-only；设计 §4.2）。
//
// 提供三种实现：
//   - FileJournal：生产实现（分段文件 + 轮转 + 原子重写 + 目录 0700 / 文件 0600）；
//   - MemoryJournal：测试与「Memory 策略」用（只进内存，显式放弃持久化）；
//   - NullJournal：禁用剖面用（no-op）。
//
// 段族（设计 §5.4.1）：同一实现 + 同一格式，物理**分族** ——
//   - 合规族：LayoutGlobal（全局时间线，便于合规导出/检索）；
//   - 任务/WAL 族：LayoutPerScope（便于 Resume 与单任务归档）。
// 容量上限/有界丢弃**只作用于合规族**；任务族以「不丢失」为硬约束（不足时拒绝新 Open）。

// ErrClosed 表示 Journal 已关闭。
var ErrClosed = errors.New("audit: journal closed")

// SegmentInfo 描述一个段文件（用于跨热/冷段定位与归档摘要，设计 §5.5）。
type SegmentInfo struct {
	Path   string    // 段文件路径
	Scope  ScopeID   // LayoutPerScope 时为所属作用域；LayoutGlobal 时为空
	Sealed bool      // 是否已封段（轮转后为 true）
	Size   int64     // 字节数
	Count  int       // 条目数（读取时统计；0 表示未统计）
	First  time.Time // 最早条目时间（未统计时零值）
	Last   time.Time // 最晚条目时间
}

// Journal 是审计条目的持久化后端（append-only）。
// 契约：Append 返回 nil ⇒ 条目**已按 Journal 的落盘策略持久化**（SyncAlways 已 fsync）。
type Journal interface {
	// Append 追加一条条目（**唯一写入口**）。
	Append(e Entry) error
	// Read 按过滤条件读取（排序语义：先 Node 后 Seq，稳定）。
	Read(f EntryFilter) ([]Entry, error)
	// Segments 返回段清单（含未统计条目数的段）。
	Segments() ([]SegmentInfo, error)
	// Rotate 封当前段并开新段（reason 写入轮转标记，见设计 §5.3）。
	Rotate(reason string) error
	// Rewrite 按谓词剔行（TTL/清理的**唯一**入口；内存与磁盘在同一事务内完成）。
	Rewrite(keep func(Entry) bool) error
	// Flush 把缓冲写盘（SyncBatch/SyncNone 策略下使用）。
	Flush() error
	// Close 关闭（幂等）。
	Close() error
}

// ---------------------------------------------------------------- MemoryJournal

// MemoryJournal 是内存实现（测试 + 「Memory 策略」：显式放弃持久化）。
type MemoryJournal struct {
	mu      sync.Mutex
	entries []Entry
	closed  bool
	rotates []string // 轮转原因（可观测/测试断言）
}

// NewMemoryJournal 创建内存 Journal。
func NewMemoryJournal() *MemoryJournal { return &MemoryJournal{} }

// Append 追加条目。
func (j *MemoryJournal) Append(e Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	j.entries = append(j.entries, e)
	return nil
}

// Read 读取（排序语义与 FileJournal 一致）。
func (j *MemoryJournal) Read(f EntryFilter) ([]Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	out := make([]Entry, 0, len(j.entries))
	for _, e := range j.entries {
		if f.Match(e) {
			out = append(out, e)
		}
	}
	sortEntries(out)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Segments 返回单一虚拟段。
func (j *MemoryJournal) Segments() ([]SegmentInfo, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	return []SegmentInfo{{Path: "memory", Count: len(j.entries)}}, nil
}

// Rotate 记录轮转原因（内存实现无物理段，仅可观测）。
func (j *MemoryJournal) Rotate(reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	j.rotates = append(j.rotates, reason)
	return nil
}

// Rewrite 按谓词剔行。
func (j *MemoryJournal) Rewrite(keep func(Entry) bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	kept := j.entries[:0]
	for _, e := range j.entries {
		if keep(e) {
			kept = append(kept, e)
		}
	}
	j.entries = kept
	return nil
}

// Flush 内存实现无需落盘。
func (j *MemoryJournal) Flush() error { return nil }

// Close 关闭。
func (j *MemoryJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closed = true
	return nil
}

// Rotates 返回轮转原因序列（测试断言用）。
func (j *MemoryJournal) Rotates() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.rotates...)
}

// ---------------------------------------------------------------- NullJournal

// NullJournal 是 no-op 实现（禁用剖面）。
type NullJournal struct{}

// Append 静默成功（禁用剖面：审计不落盘且不阻断业务）。
func (NullJournal) Append(Entry) error { return nil }

// Read 返回空。
func (NullJournal) Read(EntryFilter) ([]Entry, error) { return nil, nil }

// Segments 返回空。
func (NullJournal) Segments() ([]SegmentInfo, error) { return nil, nil }

// Rotate 静默成功。
func (NullJournal) Rotate(string) error { return nil }

// Rewrite 静默成功。
func (NullJournal) Rewrite(func(Entry) bool) error { return nil }

// Flush 静默成功。
func (NullJournal) Flush() error { return nil }

// Close 静默成功。
func (NullJournal) Close() error { return nil }

// ---------------------------------------------------------------- FileJournal

// FileJournalConfig 是 FileJournal 的构造参数。
type FileJournalConfig struct {
	// Dir 是段文件目录（以 0700 创建并校验私有；设计 M1-12）。
	Dir string
	// Layout 决定段文件组织方式（默认 LayoutGlobal）。
	Layout Layout
	// Scope 限定本 Journal 的**作用域**（LayoutPerScope 时只读写该作用域的段文件）。
	// 必须设置：同一目录下多任务共存时，否则 Read 会读到**其它作用域**的行
	// （旧 FileSink 是 per-scope 文件 + per-scope 读取，隔离语义必须保留）。
	Scope ScopeID
	// SyncMode 决定落盘强度（默认 SyncAlways：合规与 WAL 证据级）。
	SyncMode SyncMode
	// BatchLines / BatchInterval 仅 SyncBatch 生效（0 ⇒ 默认 256 行 / 200ms）。
	BatchLines    int
	BatchInterval time.Duration
	// MaxSize / MaxArchives 是段大小轮转阈值与归档份数（0 ⇒ 不轮转 / 不留档）。
	MaxSize     int64
	MaxArchives int
	// MaxTotalBytes 是**全部段（热 + 归档）总字节硬上限**（设计 §5.4 T8 / E15 / M1-8）。
	// 0 ⇒ 不限制（不推荐：审核 P1-3 要求**非零安全默认**，装配层按 4×local_reserve 提供）。
	// 达上限 ⇒ 先删**最旧归档**（有界保留）；删尽仍放不下（如单段已超上限）⇒ **有界丢弃**
	// 本次条目（计入 Metrics.IncrDrop，不报错——审计是 best-effort，绝不因满盘阻塞业务）。
	MaxTotalBytes int64
	// GlobalName 是 LayoutGlobal 下的段文件名（默认 "compliance.audit.log"）。
	GlobalName string
	// OnSeal 是**封段回调**（M4：轮转产生新封段后触发，用于归档）。
	//
	// 契约（三条，缺一不可）：
	//   - **在 Journal 锁之外**调用（回调内可安全调 `Segments()`/`ArchiveSealed`，不会自死锁）；
	//   - **同步**执行 ⇒ 必须快，或实现方自行单飞/限流（本回调在写路径上）；
	//   - **不得**再向同一 Journal 写入（否则递归轮转 ✗）。
	// 空段轮转（无内容可归档）**不触发**；多次轮转合并为一次回调。
	OnSeal func()
	// Clock / Metrics / Logger 可注入（测试确定性 + 可观测）。
	Clock   Clock
	Metrics Metrics
	Logger  *slog.Logger
}

// SyncMode 是落盘强度（设计 §5.2：包提供策略族，使用方按场景选择）。
type SyncMode string

const (
	// SyncAlways 每行 fsync（合规证据 / WAL 终态）。
	SyncAlways SyncMode = "always"
	// SyncBatch 批量 + 阈值/定时 flush（常规事件 / span）。
	SyncBatch SyncMode = "batch"
	// SyncNone 仅缓冲（高频采样）。
	SyncNone SyncMode = "none"
)

// FileJournal 是分段文件实现（append-only + 轮转 + 原子重写）。
type FileJournal struct {
	// archiveUnavailable 为 true 表示「归档已配置但目的地不可用」⇒ 轮转时**不**按
	// `max_archives` 删除最旧封段（保留证据；磁盘仍受 `max_total_bytes` 约束）。
	// 由装配层在卷集合就绪后调用 SetArchiveUnavailable 设置（评审 R13 P2）。
	archiveUnavailable atomic.Bool
	dir                string
	layout             Layout
	scope              ScopeID
	syncMode           SyncMode
	batchLines         int
	batchIv            time.Duration
	maxSize            int64
	maxArch            int
	maxTotal           int64
	globalName         string
	clock              Clock
	metrics            Metrics
	logger             *slog.Logger

	onSeal      func()
	sealPending bool // 有封段尚未通知（在锁外触发后清零）

	mu     sync.Mutex
	files  map[string]*os.File
	unsync int // SyncBatch 下未 flush 的行数
	// pendingFlush 标记「本次写入后需要 fsync」（SyncBatch 满 batchLines 时置位）。
	// 由 `Append` 在**锁外**执行 fsync（R14 C14：此前在 mu 持锁中 fsync，批量删除时把
	// 全局写路径串行化）。
	pendingFlush bool
	// syncHook 仅供测试观察批量 fsync 发生（nil = 生产无副作用）。
	syncHook func(path string)
	// batchStop 关闭后批量定时刷盘循环退出（SyncBatch 专用）。
	batchStop chan struct{}
	closed    bool
}

// NewFileJournal 创建文件 Journal（目录 0700 + 段文件 0600）。
func NewFileJournal(cfg FileJournalConfig) (*FileJournal, error) {
	if cfg.Dir == "" {
		return nil, errors.New("audit: FileJournal 需要 Dir")
	}
	if err := ensurePrivateDir(cfg.Dir); err != nil {
		return nil, err
	}
	j := &FileJournal{
		dir:        cfg.Dir,
		layout:     cfg.Layout,
		scope:      cfg.Scope,
		syncMode:   cfg.SyncMode,
		batchLines: cfg.BatchLines,
		batchIv:    cfg.BatchInterval,
		maxSize:    cfg.MaxSize,
		maxArch:    cfg.MaxArchives,
		maxTotal:   cfg.MaxTotalBytes,
		globalName: cfg.GlobalName,
		onSeal:     cfg.OnSeal,
		clock:      cfg.Clock,
		metrics:    cfg.Metrics,
		logger:     cfg.Logger,
		files:      make(map[string]*os.File),
	}
	if j.layout == "" {
		j.layout = LayoutGlobal
	}
	if j.syncMode == "" {
		j.syncMode = SyncAlways
	}
	if j.batchLines <= 0 {
		j.batchLines = 256
	}
	if j.batchIv <= 0 {
		j.batchIv = 200 * time.Millisecond
	}
	if j.globalName == "" {
		j.globalName = "compliance.audit.log"
	}
	// SyncBatch：启动**时间维度**批量刷盘（R14 C14）。
	if j.syncMode == SyncBatch {
		j.batchStop = make(chan struct{})
		go j.batchFlushLoop(j.batchIv)
	}
	if j.clock == nil {
		j.clock = systemClock{}
	}
	if j.metrics == nil {
		j.metrics = NullMetrics{}
	}
	return j, nil
}

// Dir 返回段目录（只读访问器；白盒测试不再触碰非导出字段）。
func (j *FileJournal) Dir() string { return j.dir }

// segmentPath 计算条目所属段文件路径（按 Layout 分族）。
func (j *FileJournal) segmentPath(e Entry) (string, error) {
	if j.layout == LayoutPerScope {
		// 作用域绑定的 Journal：条目作用域为空则补为本 Journal 的作用域；非空则必须一致
		// （防「把 A 任务的行写进 B 任务的段」）。
		if e.Scope == "" {
			e.Scope = j.scope
		}
		if j.scope != "" && e.Scope != j.scope {
			return "", fmt.Errorf("audit: 条目作用域 %q 与本 Journal 作用域 %q 不一致", e.Scope, j.scope)
		}
		if !validScopeID(e.Scope) {
			return "", fmt.Errorf("audit: 非法 scopeID %q（段文件按作用域命名）", e.Scope)
		}
		return filepath.Join(j.dir, string(e.Scope)+".audit.log"), nil
	}
	return filepath.Join(j.dir, j.globalName), nil
}

// Append 追加条目（按策略落盘；超 MaxSize 时先轮转，随后在**锁外**触封段回调）。
func (j *FileJournal) Append(e Entry) error {
	err := j.appendLocked(e)
	// R14 C14：批量 fsync 移出锁（此前在 `mu` 持锁中同步 ⇒ 批量删除时全局写路径被串行化）。
	j.flushPendingOutOfLock()
	j.fireSealIfPending()
	return err
}

// flushPendingOutOfLock 把「已写但未 fsync」的段刷盘（锁内只做快照，fsync 在锁外）。
// 并发 Close 会关闭文件 ⇒ 我们对已取快照的 *os.File 调 Sync 只可能返回错误，不会 panic。
func (j *FileJournal) flushPendingOutOfLock() {
	j.mu.Lock()
	if !j.pendingFlush || j.closed {
		j.mu.Unlock()
		return
	}
	j.pendingFlush = false
	files := make(map[string]*os.File, len(j.files))
	maps.Copy(files, j.files)
	hook := j.syncHook
	j.mu.Unlock()

	for p, f := range files {
		if serr := f.Sync(); serr != nil {
			j.noteWriteFailure(serr)
			continue
		}
		if hook != nil {
			hook(p)
		}
	}
}

// batchFlushLoop 是 SyncBatch 的**时间维度**刷盘（R14 C14：此前只有 256 行阈值 ⇒ kill -9 前
// 最多 255 条证据停在 page cache，与设计「≤ batch_max_interval 一次 fsync」不符）。
func (j *FileJournal) batchFlushLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-j.batchStop:
			return
		case <-t.C:
			j.mu.Lock()
			if j.closed {
				j.mu.Unlock()
				return
			}
			if j.unsync > 0 {
				j.unsync = 0
				j.pendingFlush = true
			}
			j.mu.Unlock()
			j.flushPendingOutOfLock()
		}
	}
}

// appendLocked 是 Append 的持锁主体（拆出只为把封段回调放到锁外）。
func (j *FileJournal) appendLocked(e Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	if e.Scope == "" {
		e.Scope = j.scope
	}
	path, err := j.segmentPath(e)
	if err != nil {
		return err
	}
	if err := j.rotateIfTooBigLocked(path); err != nil {
		return err
	}
	if dropped, err := j.enforceTotalLocked(path); err != nil || dropped {
		return err // dropped ⇒ 本次条目按**有界丢弃**处理（已计数），不视为写入失败
	}
	return j.writeEntryLocked(path, e)
}

// rotateIfTooBigLocked 在段超 MaxSize 时先轮转（0 ⇒ 不轮转）。
func (j *FileJournal) rotateIfTooBigLocked(path string) error {
	if j.maxSize <= 0 {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() < j.maxSize {
		return nil
	}
	if rerr := j.rotateLocked(path, "size"); rerr != nil {
		j.noteWriteFailure(rerr)
		return rerr
	}
	return nil
}

// enforceTotalLocked 在写入前把「全部段（热 + 归档）总字节」压到 MaxTotalBytes 内（M1-8 /
// 设计 §5.4.1 T8、E15）：① 未超 ⇒ 放行；② 超 ⇒ 从**最旧归档**（.N 最大者）开始删除
// （有界保留，计入 IncrEvict —— 已持久化数据的**受控淘汰**，不是丢失）；③ 归档删尽仍放不下
// （如单段已超上限）⇒ **有界丢弃**本次条目并返回 dropped=true（计入 IncrDrop）。
//
// 语义边界（设计审核 P1）：本函数只作用于**合规段族**的 Journal 实例；任务/WAL 段族以
// 「不丢失」为硬约束（调用方不配置 MaxTotalBytes ⇒ 不触发本治理）。
func (j *FileJournal) enforceTotalLocked(incomingPath string) (dropped bool, err error) {
	if j.maxTotal <= 0 {
		return false, nil
	}
	total, arch, terr := j.totalAndArchivesLocked()
	if terr != nil {
		return false, terr
	}
	if total <= j.maxTotal {
		return false, nil
	}
	// 从最旧归档删起（archiveIndex 大者更旧）。
	sort.Slice(arch, func(a, b int) bool { return archiveIndex(arch[a].Path) > archiveIndex(arch[b].Path) })
	for _, s := range arch {
		if total <= j.maxTotal {
			break
		}
		if rmErr := os.Remove(s.Path); rmErr == nil {
			total -= s.Size
			j.metrics.IncrEvict(1)
		}
	}
	if total > j.maxTotal {
		// 归档删尽仍超限（如热段本身已达上限）：**丢弃本次条目**（有界丢弃，不阻塞业务）。
		j.metrics.IncrDrop(1)
		return true, nil
	}
	_ = incomingPath
	return false, nil
}

// totalAndArchivesLocked 返回全部段总字节与**归档段**列表（调用方须持 j.mu）。
func (j *FileJournal) totalAndArchivesLocked() (int64, []SegmentInfo, error) {
	names, err := j.segmentFilesLocked()
	if err != nil {
		return 0, nil, err
	}
	var total int64
	arch := make([]SegmentInfo, 0, len(names))
	for _, name := range names {
		st, serr := os.Stat(name)
		if serr != nil {
			continue
		}
		total += st.Size()
		base := filepath.Base(name)
		if segmentBase(base) != base { // 归档 .N ⇒ 已封段
			arch = append(arch, SegmentInfo{Path: name, Sealed: true, Size: st.Size()})
		}
	}
	return total, arch, nil
}

// archiveIndex 解析归档名中**紧跟 `audit.log.` 的序号**（`audit.log.3` ⇒ 3；非归档 ⇒ 0）。
//
// 为什么要「紧跟 audit.log.」而不是「最后一个点后的数字」：冷段归档采用**内容寻址**命名
// （`audit.log.3.<sha256:16>`，见 `FSArchiver.Archive`）⇒ 名字末尾是哈希而不是序号；
// 若仍取最后一个点后的片段，所有冷段的 Index 都会退化成 0 ⇒ `ListCold` 的「最旧优先」排序
// 与读面热冷合并的顺序都会退化成不确定 ✗（评审 R13 修复过程中实测）。
func archiveIndex(path string) int {
	base := filepath.Base(path)
	const marker = "audit.log."
	i := strings.LastIndex(base, marker)
	if i < 0 {
		return 0
	}
	rest := base[i+len(marker):]
	if cut := strings.IndexByte(rest, '.'); cut >= 0 {
		rest = rest[:cut] // 丢弃内容寻址后缀（或二次轮转残留）
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// SetArchiveUnavailable 标记「归档已配置但目的地不可用」（装配层设置；并发安全）。
func (j *FileJournal) SetArchiveUnavailable(v bool) { j.archiveUnavailable.Store(v) }

// QuarantineSegment 在**锁内**把封段改名为归档专用的独占名（`<name>.archiving.<n>`）。
//
// 为什么需要（评审 R13 P2）：归档是「打开段文件 → 上传（可能很久）→ 删除该路径」。期间若发生
// 轮转，`shiftArchives` 会把 `.1` 搬成 `.2` 并产生**新的** `.1`；此时按原路径 `os.Remove` 删除的
// 是**新段**（尚未归档）⇒ 证据丢失。改名为独占名后：轮转只按 `base.N` 形态匹配（不会碰带后缀的
// 隔离名），删除时也只删隔离名 ⇒ 与轮转彻底解耦。
//
// 返回隔离后的路径；段已不存在/改名失败则返回错误（调用方跳过本轮，下轮重试）。
func (j *FileJournal) QuarantineSegment(path string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return "", ErrClosed
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	q := fmt.Sprintf("%s.archiving.%d", path, j.clock.Now().UnixNano())
	if err := os.Rename(path, q); err != nil {
		return "", err
	}
	return q, nil
}

// writeEntryLocked 序列化并写入段文件（按 SyncMode 落盘）。
func (j *FileJournal) writeEntryLocked(path string, e Entry) error {
	line, merr := json.Marshal(e)
	if merr != nil {
		return fmt.Errorf("audit: 条目序列化失败: %w", merr)
	}
	f, oerr := j.openLocked(path)
	if oerr != nil {
		j.noteWriteFailure(oerr)
		return oerr
	}
	if _, werr := f.Write(append(line, 0x0a)); werr != nil {
		j.noteWriteFailure(werr)
		return werr
	}
	if j.syncMode == SyncBatch {
		j.unsync++
		if j.unsync < j.batchLines {
			return nil
		}
		j.unsync = 0
		j.pendingFlush = true // 锁外统一 fsync（见 Append）
		return nil
	}
	if j.syncMode == SyncNone {
		return nil
	}
	if serr := f.Sync(); serr != nil {
		j.noteWriteFailure(serr)
		return serr
	}
	return nil
}

// openLocked 懒打开段文件（0600）。
func (j *FileJournal) openLocked(path string) (*os.File, error) {
	if f, ok := j.files[path]; ok {
		return f, nil
	}
	// 复用 openAuditFile：Unix 下 O_NOFOLLOW 原子拒符号链接；非 Unix 下先 Lstat 校验
	// （**审计文件写入目标可预测 ⇒ 必须防重定向**，与旧 FileSink 同一安全契约）。
	f, err := openAuditFile(path)
	if err != nil {
		return nil, err
	}
	j.files[path] = f
	return f, nil
}

// noteWriteFailure 记录落盘失败（可观测；不 panic）。
func (j *FileJournal) noteWriteFailure(err error) {
	j.metrics.IncrWriteFailed()
	if j.logger != nil {
		j.logger.Warn("audit: 段文件写入失败", "dir", j.dir, "err", err)
	}
}

// Read 读取全部段并按 (Node, Seq) 排序后过滤。
//
// **不在读期间持写锁**（评审 R13 P1）：`Append` 需要同一把 `j.mu`，此前整段解析 + 排序都在
// 锁内 ⇒ 一次管理员查询（`/api/audit`）会把**全部**变更型请求的审计写入阻塞到读完为止。
// 现在只在锁内做「flush 未同步缓冲 + 快照段文件名」，解析/过滤/排序在锁外进行。
//
// 语义代价（有意取舍）：读到的是**快照时刻的段列表** —— 解析期间若发生轮转/归档，少数段可能
// 已被删除 ⇒ `readSegmentFile` 失败按既有语义跳过（E5/E20），不会返回错误，也不会读到半个条目。
func (j *FileJournal) Read(f EntryFilter) ([]Entry, error) {
	return j.ReadContext(context.Background(), f)
}

// ReadContext 是 Read 的 ctx 版本（长日志解析期间可取消）。
func (j *FileJournal) ReadContext(ctx context.Context, f EntryFilter) ([]Entry, error) {
	names, err := j.snapshotSegments()
	if err != nil {
		return nil, err
	}
	entries := j.readSegments(ctx, names)
	return filterSortLimit(entries, f, 0), nil
}

// ReadTail 返回**最新的 limit 条**（结果仍按 (Node, Seq) **升序**返回），只解析必要的段。
//
// 动机（评审 R13 P1）：`/api/audit?limit=N` 之前要把**全部**历史解析进内存再截断 ——
// 审计日志默认不轮转，可达 GiB 级 ⇒ 内存/CPU 随历史线性增长。这里按「新段优先」反向解析，
// 收集到 limit 条即停止；未命中过滤条件的段仍需读（过滤在解析后），但**已满足预算的段不再读**。
func (j *FileJournal) ReadTail(ctx context.Context, f EntryFilter, limit int) ([]Entry, error) {
	if limit <= 0 {
		return j.ReadContext(ctx, f)
	}
	names, err := j.snapshotSegments()
	if err != nil {
		return nil, err
	}
	// 新段优先：按归档序号**升序**——活跃段 archiveIndex=0 排最前，封段 `.1` 最新、`.N` 最旧
	// （`shiftArchives` 把 `.1`→`.2`，越大越旧）。
	//
	// P1 修复（2026-10-21）：此前是**降序**，于是先读 `.N`（最旧）并在凑满 limit 后 break ⇒
	// `/api/audit?limit=N` 在热目录仍有封段时返回的是**最旧**的 N 条，最新事件完全不可见
	// （且 `skippable=true` 会连带跳过冷段）。单段场景下顺序无意义，故此前测试未发现。
	sort.SliceStable(names, func(a, b int) bool {
		return archiveIndex(names[a]) < archiveIndex(names[b])
	})
	var got []Entry
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, e := range readSegmentFileCtx(ctx, name, j.logger) {
			if f.Match(e) {
				got = append(got, e)
			}
		}
		if len(got) >= limit {
			break
		}
	}
	sortEntries(got)
	if len(got) > limit {
		got = got[len(got)-limit:] // 取最新 limit 条（排序为升序 ⇒ 尾部最新）
	}
	return got, nil
}

// snapshotSegments 在锁内 flush 未同步缓冲并返回段文件名快照（随后解析在锁外进行）。
func (j *FileJournal) snapshotSegments() ([]string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	if j.unsync > 0 {
		for _, f := range j.files {
			if err := f.Sync(); err != nil {
				j.noteWriteFailure(err)
			}
		}
		j.unsync = 0
	}
	return j.segmentFilesLocked()
}

// readSegments 逐个解析段文件（读取失败按既有语义跳过并记日志）。
func (j *FileJournal) readSegments(ctx context.Context, names []string) []Entry {
	var all []Entry
	for _, name := range names {
		if ctx.Err() != nil {
			return all
		}
		all = append(all, readSegmentFileCtx(ctx, name, j.logger)...)
	}
	return all
}

// readSegmentFileCtx 是 readSegmentFile 的 ctx 版本（解析失败跳过并记日志）。
func readSegmentFileCtx(ctx context.Context, path string, logger *slog.Logger) []Entry {
	if ctx.Err() != nil {
		return nil
	}
	es, err := readSegmentFile(path)
	if err != nil {
		if logger != nil {
			logger.Warn("audit: 段文件读取失败（跳过）", "path", path, "err", err)
		}
		return nil
	}
	return es
}

// filterSortLimit 过滤 → 排序 → 截断（limit<=0 不截断）。
func filterSortLimit(entries []Entry, f EntryFilter, limit int) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if f.Match(e) {
			out = append(out, e)
		}
	}
	sortEntries(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// segmentFilesLocked 列出本目录下的段文件（含归档 .N）。
// 匹配规则（与 Layout 对齐，且必须认**配置的全局段名**）：
//   - LayoutGlobal：name == GlobalName（或其归档 GlobalName.N）；
//   - LayoutPerScope：name == <scope>.audit.log（或其归档 .N）；作用域绑定时只列本作用域。
func (j *FileJournal) segmentFilesLocked() ([]string, error) {
	des, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		if !j.matchSegmentName(de.Name()) {
			continue
		}
		out = append(out, filepath.Join(j.dir, de.Name())) // **真实文件名**（保留归档后缀）
	}
	sort.Strings(out)
	return out, nil
}

// matchSegmentName 判定目录项是否属于本 Journal 的段（含归档 .N）。
func (j *FileJournal) matchSegmentName(name string) bool {
	base := segmentBase(name)
	if j.layout == LayoutPerScope {
		if !strings.HasSuffix(base, ".audit.log") {
			return false
		}
		if j.scope != "" && !strings.HasPrefix(base, string(j.scope)+".audit.log") {
			return false
		}
		return true
	}
	return base == j.globalName
}

// segmentBase 返回段基名（去掉归档 .N 后缀；非归档返回原名）。
func segmentBase(name string) string {
	idx := strings.LastIndex(name, ".")
	if idx <= 0 {
		return name
	}
	if _, err := strconv.Atoi(name[idx+1:]); err != nil {
		return name
	}
	if !strings.Contains(name[:idx], ".log") {
		return name
	}
	return name[:idx]
}

// readSegmentFile 逐行解析段文件（**丢弃不完整尾行与解析失败行**；设计 E5）。
func readSegmentFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	rd := bufio.NewReader(f)
	for {
		line, rerr := rd.ReadBytes(0x0a)
		if e, ok := decodeSegmentLine(line); ok {
			out = append(out, e)
		}
		if rerr == nil {
			continue
		}
		if rerr == io.EOF {
			return out, nil
		}
		return out, rerr
	}
}

// decodeSegmentLine 解码一行段记录：无换行结尾（崩溃截断）或 JSON 解析失败 ⇒ ok=false。
func decodeSegmentLine(line []byte) (Entry, bool) {
	if len(line) == 0 || line[len(line)-1] != 0x0a {
		return Entry{}, false
	}
	var e Entry
	if err := json.Unmarshal(line[:len(line)-1], &e); err != nil {
		return Entry{}, false
	}
	return e, true
}

// Segments 返回段清单。
func (j *FileJournal) Segments() ([]SegmentInfo, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, ErrClosed
	}
	names, err := j.segmentFilesLocked()
	if err != nil {
		return nil, err
	}
	out := make([]SegmentInfo, 0, len(names))
	for _, name := range names {
		st, serr := os.Stat(name)
		if serr != nil {
			continue
		}
		base := filepath.Base(name)
		sealed := segmentBase(base) != base // 归档 .N ⇒ 已封段
		scope := ScopeID("")
		if j.layout == LayoutPerScope && !sealed {
			scope = ScopeID(strings.TrimSuffix(base, ".audit.log"))
		}
		out = append(out, SegmentInfo{Path: name, Scope: scope, Sealed: sealed, Size: st.Size()})
	}
	return out, nil
}

// Rotate 封当前段（按原因；设计 §5.3 的段切换：先确保新段可写，再移旧段），
// 随后在**锁外**触封段回调。
func (j *FileJournal) Rotate(reason string) error {
	err := j.rotateAllLocked(reason)
	j.fireSealIfPending()
	return err
}

// rotateAllLocked 是 Rotate 的持锁主体（拆出只为把封段回调放到锁外）。
func (j *FileJournal) rotateAllLocked(reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	names, err := j.segmentFilesLocked()
	if err != nil {
		return err
	}
	rotated := false
	for _, name := range names {
		// 归档段判定必须用 `archiveIndex`：生产段名是 `audit.log`（不含前置点），旧的
		// `Contains(name, ".audit.log.")` 只对默认段名 `compliance.audit.log` 成立 ⇒ 生产上会把
		// `audit.log.1` 当在线段再轮转成 `audit.log.1.1`（该名字的 segmentBase 不再是 globalName
		// ⇒ 从 Segments()/Read() 里消失，磁盘仍占）✗ 评审 R13 P2。
		if archiveIndex(filepath.Base(name)) > 0 {
			continue // 归档段不重复轮转
		}
		if rerr := j.rotateLocked(name, reason); rerr != nil {
			return rerr
		}
		rotated = true
	}
	if rotated {
		j.metrics.IncrRotate(reason)
	}
	return nil
}

// rotateLocked 关闭并归档一个段（.1 最新，超 maxArchives 删除最旧）。
func (j *FileJournal) rotateLocked(path, reason string) error {
	j.closeSegmentLocked(path)
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		return nil // 空段无需归档（避免制造空归档文件）
	}
	j.shiftArchives(path)
	if err := os.Rename(path, archivePath(path, 1)); err != nil {
		return err
	}
	j.sealPending = true // 产生了新封段：等锁外统一通知（空段在上面已 early return）
	if j.logger != nil {
		j.logger.Info("audit: 段轮转", "path", path, "reason", reason)
	}
	return nil
}

// fireSealIfPending 在**锁外**触封段回调（一次合并多轮轮转；回调 panic 被隔离，绝不影响写入）。
func (j *FileJournal) fireSealIfPending() {
	j.mu.Lock()
	pend := j.sealPending
	j.sealPending = false
	cb := j.onSeal
	j.mu.Unlock()
	if !pend || cb == nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil && j.logger != nil {
				j.logger.Error("audit: 封段回调 panic（已隔离）", "panic", r)
			}
		}()
		cb()
	}()
}

// closeSegmentLocked 同步并关闭段句柄（不存在则跳过）。
func (j *FileJournal) closeSegmentLocked(path string) {
	f, ok := j.files[path]
	if !ok {
		return
	}
	if err := f.Sync(); err != nil {
		j.noteWriteFailure(err)
	}
	_ = f.Close()
	delete(j.files, path)
}

// shiftArchives 把 .N 归档依次后移（.maxArchives 丢弃）。
func (j *FileJournal) shiftArchives(path string) {
	if j.maxArch <= 0 {
		return
	}
	// 归档**已配置但不可用**：整体跳过搬移与删除。
	//   · 若只跳过「删除」这一步，后面的 `.1 → .2` rename 仍会**覆盖**最旧的 `.2`（Unix 语义）
	//     ⇒ 证据照样丢 ✗；
	//   · 跳过整轮 ⇒ 封段只增不减，磁盘由 `max_total_bytes` 兜底（有界丢弃可观测）。
	if j.archiveUnavailable.Load() {
		if j.logger != nil {
			j.logger.Warn("audit: 归档不可用 ⇒ 跳过封段搬移/回收（保留证据，受 max_total_bytes 约束）",
				"path", path, "max_archives", j.maxArch)
		}
		return
	}
	for i := j.maxArch; i >= 1; i-- {
		src := archivePath(path, i)
		if i == j.maxArch {
			_ = os.Remove(src)
			continue
		}
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, archivePath(path, i+1))
		}
	}
}

// archivePath 返回归档路径（<name>.N）。
func archivePath(path string, n int) string { return path + "." + strconv.Itoa(n) }

// Rewrite 按谓词剔行（**内存与磁盘在同一事务内完成**；TTL/清理唯一入口）。
func (j *FileJournal) Rewrite(keep func(Entry) bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	names, err := j.segmentFilesLocked()
	if err != nil {
		return err
	}
	for _, name := range names {
		if archiveIndex(filepath.Base(name)) > 0 {
			continue // 归档段为离线冷数据，不参与在线重写（设计 E25/§5.5）
		}
		if err := j.rewriteSegmentLocked(name, keep); err != nil {
			return err
		}
	}
	return nil
}

// rewriteSegmentLocked 原子重写单个段（temp + rename）。
func (j *FileJournal) rewriteSegmentLocked(path string, keep func(Entry) bool) error {
	entries, err := readSegmentFile(path)
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if keep(e) {
			kept = append(kept, e)
		}
	}
	if f, ok := j.files[path]; ok {
		_ = f.Close()
		delete(j.files, path)
	}
	tmp := path + ".rewrite.tmp"
	f, cerr := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if cerr != nil {
		return cerr
	}
	w := bufio.NewWriter(f)
	for _, e := range kept {
		line, merr := json.Marshal(e)
		if merr != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return merr
		}
		if _, werr := w.Write(append(line, '\n')); werr != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return werr
		}
	}
	if ferr := w.Flush(); ferr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return ferr
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return serr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, path)
}

// Flush 把缓冲写盘。
func (j *FileJournal) Flush() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	for path, f := range j.files {
		if err := f.Sync(); err != nil {
			j.noteWriteFailure(err)
			return err
		}
		_ = path
	}
	j.unsync = 0
	return nil
}

// Close 关闭全部段句柄（幂等）。
func (j *FileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if j.batchStop != nil {
		close(j.batchStop) // 批量定时刷盘循环退出（幂等由 closed 标志保证）
	}
	var firstErr error
	for path, f := range j.files {
		if err := f.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(j.files, path)
	}
	return firstErr
}

// sortEntries 按 (Node, Seq) 稳定排序（设计 §4.1：全局序 = (Node, 节点级 seq)；
// 单机模型下 Node 恒同值 ⇒ 退化为节点 seq）。TS 仅用于展示与 Since 过滤。
func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, k int) bool {
		if es[i].Node != es[k].Node {
			return es[i].Node < es[k].Node
		}
		if es[i].Seq != es[k].Seq {
			return es[i].Seq < es[k].Seq
		}
		return es[i].TS.Before(es[k].TS)
	})
}

// ---------------------------------------------------------------- 有界 max(seq) 查询

// journalMaxSeq 返回某作用域已知的最大 Seq（ok=false 表示「无法低成本确定」，调用方回退
// 全量 Read）。**动机（P1，2026-10-21）**：`Service.seqFloor` 原本对每个作用域 `Read` 整个
// 历史并取 max——生产合规段族是单文件全局段且默认**不轮转**（上限 `max_total_bytes` 默认
// 1GiB）⇒ 进程重启后的**第一条审计写入**会把 1GiB 段全量解析进内存（数百万条带 Meta 的
// Entry，峰值 GB 级）并同步阻塞该请求，小容器直接 OOM。
//
// 实现：FileJournal 从**最新段**开始反向扫描（段内按行逆序），命中该作用域第一条即返回
// （Seq 单调 ⇒ 第一条就是最大）；内存占用与段大小无关（256KiB 块 + 单行 carry）。
func journalMaxSeq(j Journal, scope ScopeID) (uint64, bool) {
	if scope == "" {
		return 0, true
	}
	switch jj := j.(type) {
	case *FileJournal:
		names, err := jj.snapshotSegments()
		if err != nil {
			return 0, false
		}
		// 新段优先：archiveIndex 升序（活跃段 0 最前，封段 .1 最新、.N 最旧）。
		sort.SliceStable(names, func(a, b int) bool {
			return archiveIndex(names[a]) < archiveIndex(names[b])
		})
		for _, name := range names {
			if max, ok := maxSeqInSegmentFile(name, scope); ok {
				return max, true
			}
		}
		return 0, true // 扫完所有段都没有该作用域 ⇒ 起点 0（与旧语义一致）
	case *MemoryJournal:
		jj.mu.Lock()
		defer jj.mu.Unlock()
		var max uint64
		for i := range jj.entries {
			if jj.entries[i].Scope == scope && jj.entries[i].Seq > max {
				max = jj.entries[i].Seq
			}
		}
		return max, true
	default:
		return 0, false
	}
}

// maxSeqInSegmentFile 从段文件**末尾**反向按行扫描，返回首个匹配 scope 条目的 Seq。
// 内存有界（chunk + carry）；行内的 `carry` 是「跨块行」的前半段（属于更旧内容）。
func maxSeqInSegmentFile(path string, scope ScopeID) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return 0, false
	}
	const chunk = 256 << 10
	var carry []byte // 上一（更新）块的**首段残行**：其前部在更旧块中
	for end := st.Size(); end > 0; {
		start := end - chunk
		atFileStart := start <= 0
		if atFileStart {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, rerr := f.ReadAt(buf, start); rerr != nil && !errors.Is(rerr, io.EOF) {
			return 0, false
		}
		var found uint64
		var ok bool
		found, ok, carry = scanChunkBackward(buf, carry, scope, atFileStart)
		if ok {
			return found, true
		}
		end = start
	}
	return 0, false
}

// scanChunkBackward 逆序扫描一个块内的行：末元素最新。`atFileStart` 时 parts[0] 是文件首行
// （也在本块内，最后处理）；否则 parts[0] 是跨块残行 ⇒ 作为下一（更旧）块的 carry 返回。
// 返回 (命中的 Seq, 是否命中, 新的 carry)。
func scanChunkBackward(buf, carry []byte, scope ScopeID, atFileStart bool) (uint64, bool, []byte) {
	data := make([]byte, 0, len(buf)+len(carry))
	data = append(append(data, buf...), carry...)
	parts := bytes.Split(data, []byte{0x0a})
	var nextCarry []byte
	if !atFileStart {
		nextCarry = append([]byte(nil), parts[0]...)
	}
	for i := len(parts) - 1; i >= 1; i-- {
		if max, ok := seqOfLine(parts[i], scope); ok {
			return max, true, nextCarry
		}
	}
	if atFileStart {
		if max, ok := seqOfLine(parts[0], scope); ok {
			return max, true, nil
		}
	}
	return 0, false, nextCarry
}

// seqOfLine 解码一行并返回匹配 scope 的 Seq。
func seqOfLine(line []byte, scope ScopeID) (uint64, bool) {
	if len(line) == 0 {
		return 0, false
	}
	e, ok := decodeSegmentLine(append(line, '\n'))
	if !ok || e.Scope != scope {
		return 0, false
	}
	return e.Seq, true
}
