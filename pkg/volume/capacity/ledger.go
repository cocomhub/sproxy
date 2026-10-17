// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ledger.go —— 卷级容量记账的**唯一实现**：跨进程分片日志账本。
//
// 设计文档：`docs/designs/2026-10-17-quota-ledger-persistence.md`。
//
// # 为什么只有一套（取代旧的两套）
//
// 本包此前有**两套**持久化计数器（`VolumeCapacityCounter` 单文件快照、`PoolCounter` + 快照），
// 都是「**单写者**」假设：进程内读-改-写，落盘靠 tmp+rename。多进程共享同一卷时二者都会
// **丢失更新**（各自只看到自己的 used ⇒ 可同时通过 ⇒ 总和超限 = "卷打满"）。
//
// 用户裁定（2026-10-17）：**避免多套同时存在导致复杂性** ⇒ 本文件是卷级容量记账的**唯一实现**：
// 旧两套已删除，不再并存 ✗。
//
// # 为什么是「分片日志」而不是「文件锁 + 读-改-写」
//
// 共享卷（NFS/S3/WebDAV/baidupcs/cluster）上的跨进程互斥不可靠（POSIX 锁跨节点语义不一致），
// 引入分布式锁会同时带来**新依赖**与**可用性下降**。改为**每个进程只写自己的分片**
// （单写者 ⇒ 无需锁），读时求和 ⇒ 无协调、无丢失更新、崩溃安全。
//
// # 目录布局（`<卷根保留前缀>/`，由调用方给定）
//
//	shard.<node>.log   本进程 append-only 增量：每行一个带符号十进制整数（人类可读、可 tail）
//	snap.<node>        本进程压缩后的基线：单个十进制整数（原子写）
//
// # 落点：**原始卷 FS**（不经 `CapacityFS`）
//
// 账本若写在 `CapacityFS` 之内，会被**同一个计数器**统计 ⇒ 每追加一行就抬高 used ⇒
// **自记账正反馈（runaway）**。故账本建在**原始** `be.FS()` 上（`WrapStorageVolume` 中取得）。
//
// # 权威性（重要）
//
// **对账（磁盘实况）是最终权威**，账本只用于在途期间的快速判定：崩溃只丢「未 flush 的增量」
// ⇒ 方向是**少计**（可能超限，不会误拒），由对账周期约束窗口。
// 跨进程的「检查-再追加」本身非原子（无锁设计的必然代价）⇒ 残余**有界**超限窗口。
//
// # 损坏语义：**fail-closed**（不 fail-open）
//
// 分片不可解析 ⇒ 无法确定真实占用 ⇒ **拒绝新写入**并计数/告警，直到调用方压缩或对账清理。
// 理由：「限额被静默放宽」是本项目认定的**最危险形态**（同旧 `CorruptSnapshotCount` 的取向）。

// ErrLedgerDegraded 表示账本损坏/不可读 ⇒ 无法确定占用 ⇒ **fail-closed** 拒绝新写入。
var ErrLedgerDegraded = errors.New("quota ledger degraded")

// 分片与基线的文件名前缀（其余部分为节点标识）。
const (
	ledgerShardPrefix = "shard."
	ledgerSnapPrefix  = "snap."
	ledgerShardSuffix = ".log"
	// LedgerDirName 是账本在**卷根**下的保留目录名（全卷语义 ⇒ 不放在 `<owner>/` 下）。
	LedgerDirName = "meta/quota"
)

// ledgerCorruptShards 是**进程级**「账本分片损坏/不可解析」计数（可观测面）。
var ledgerCorruptShards atomic.Int64

// CorruptLedgerCount 返回本进程累计的账本分片损坏次数（指标/告警面消费）。
func CorruptLedgerCount() int64 { return ledgerCorruptShards.Load() }

// ledgerDegraded 是**进程级**「账本降级 ⇒ 拒写」计数（区分「偶发损坏」与「持续拒写」）。
var ledgerDegraded atomic.Int64

// LedgerDegradedCount 返回本进程累计的「因账本降级而拒绝写入」次数。
func LedgerDegradedCount() int64 { return ledgerDegraded.Load() }

// ledgerStore 抽象账本的存储。
//
// **为何需要抽象**：账本必须与卷**同处**（跨进程可见 ⇒ 不能在节点本地盘），而卷可能是远端
// `syncpkg.FS`（s3/webdav/baidupcs/cluster）⇒ 不能直接用 `os.*`。
// 两种实现：`osStore`（本地目录，测试用）、`fsStore`（卷内 `syncpkg.FS`，生产用）。
type ledgerStore interface {
	// readFile 读文件内容（不存在 ⇒ ok=false, err=nil）。
	readFile(name string) ([]byte, bool, error)
	// appendLine 追加一行（单写者 ⇒ 无需锁）。
	appendLine(name, line string) error
	// writeAtomic 原子写（tmp + rename）。
	writeAtomic(name string, b []byte) error
	// list 列出目录内**文件名**（不含子目录）。
	list() ([]string, error)
	// truncate 清空文件（不存在 ⇒ 空操作）。
	truncate(name string) error
	// close 释放句柄（幂等）。
	close() error
}

// ---- osStore：本地目录实现（测试与本地场景）----

type osStore struct {
	dir  string
	mu   sync.Mutex
	hand map[string]*os.File
}

func newOSStore(dir string) (*osStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("capacity: 创建账本目录失败: %w", err)
	}
	return &osStore{dir: dir, hand: map[string]*os.File{}}, nil
}

func (s *osStore) readFile(name string) ([]byte, bool, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // 账本目录由调用方给定
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

func (s *osStore) appendLine(name, line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.hand[name]
	if !ok {
		var err error
		f, err = os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // 账本随卷
		if err != nil {
			return err
		}
		s.hand[name] = f
	}
	_, err := f.WriteString(line)
	return err
}

func (s *osStore) writeAtomic(name string, b []byte) error {
	p := filepath.Join(s.dir, name)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec // 账本随卷
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(s.dir)
}

func (s *osStore) list() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (s *osStore) truncate(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 关闭句柄后按**路径**截断：Windows 上对 `O_APPEND|O_WRONLY` 句柄调 `Truncate` 会
	// "Access is denied"（实测）⇒ 路径式截断跨平台可靠。
	if f, ok := s.hand[name]; ok {
		_ = f.Close()
		delete(s.hand, name)
	}
	return os.Truncate(filepath.Join(s.dir, name), 0)
}

func (s *osStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, f := range s.hand {
		_ = f.Close()
		delete(s.hand, k)
	}
	return nil
}

// ---- fsStore：卷内 syncpkg.FS 实现（生产用）----

// 为何不持有 ctx：`containedctx` 静态检查禁止长生命周期结构持有 `context.Context`（它应随请求走）。
// 而账本操作发生在 `Counter` 接口之下（`TryAdd/Release/Used` 均无 ctx）⇒ 本就无法获得请求 ctx，
// 故各方法用 `context.Background()`：语义**诚实**（账本操作不可取消，由卷 FS 自身超时兜底）。
type fsStore struct {
	fs  syncpkg.FS
	dir string
	mu  sync.Mutex
}

func newFSStore(fsys syncpkg.FS, dir string) (*fsStore, error) {
	if fsys == nil {
		return nil, errors.New("capacity: 卷内账本需要 FS")
	}
	// 建目录（幂等）：已存在时部分实现返回错误 ⇒ 以 Stat 复核，存在即接受。
	if err := fsys.MakeDir(context.Background(), dir); err != nil {
		if _, serr := fsys.Stat(context.Background(), dir); serr != nil {
			return nil, fmt.Errorf("capacity: 创建卷内账本目录失败: %w", err)
		}
	}
	return &fsStore{fs: fsys, dir: dir}, nil
}

func (s *fsStore) path(name string) string {
	if s.dir == "" {
		return name
	}
	return strings.TrimSuffix(s.dir, "/") + "/" + name
}

func (s *fsStore) readFile(name string) ([]byte, bool, error) {
	rc, err := s.fs.OpenRead(context.Background(), s.path(name))
	if err != nil {
		// FS 抽象通常无 IsNotExist 等价物 ⇒ 以 Stat 区分「不存在」（正常）与「不可读」（降级）。
		if _, serr := s.fs.Stat(context.Background(), s.path(name)); serr != nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rc.Close()
	b, rerr := io.ReadAll(rc)
	if rerr != nil {
		return nil, false, rerr
	}
	return b, true, nil
}

func (s *fsStore) appendLine(name, line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 卷 FS 无 O_APPEND 语义 ⇒ 读旧 + 整体原子重写。
	// 代价：每次追加一次读+写 ⇒ 仅**共享卷**走账本（本地卷零 I/O）。
	old, _, err := s.readFile(name)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(old)+len(line))
	buf = append(buf, old...)
	buf = append(buf, line...)
	return s.writeAtomic(name, buf)
}

func (s *fsStore) writeAtomic(name string, b []byte) error {
	return s.fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, s.path(name), bytes.NewReader(b), int64(len(b)), 0)
}

func (s *fsStore) list() ([]string, error) {
	entries, err := s.fs.ListDir(context.Background(), s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			out = append(out, e.Name)
		}
	}
	return out, nil
}

func (s *fsStore) truncate(name string) error {
	return s.fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, s.path(name), bytes.NewReader(nil), 0, 0)
}

func (s *fsStore) close() error { return nil }

// ---- Ledger：分片日志账本 ----

// Ledger 是某「配额键」（卷级容量，或某用户/桶级配额）在**共享卷**上的跨进程账本。
// 零值不可用；用 `OpenLedger`（本地目录）/`OpenLedgerFS`（卷内 FS）构造。**并发安全**。
type Ledger struct {
	mu      sync.Mutex
	store   ledgerStore
	node    string
	corrupt bool
	shards  int
}

// OpenLedger 打开本地目录上的账本（测试与本地场景）。
func OpenLedger(dir, node string) (*Ledger, error) {
	st, err := newOSStore(dir)
	if err != nil {
		return nil, err
	}
	return newLedger(st, node)
}

// OpenLedgerFS 打开**卷内**账本：账本文件经 fsys（**原始卷 FS**，非 `CapacityFS` ⇒ 不自记账）。
func OpenLedgerFS(fsys syncpkg.FS, dir, node string) (*Ledger, error) {
	st, err := newFSStore(fsys, dir)
	if err != nil {
		return nil, err
	}
	return newLedger(st, node)
}

func newLedger(st ledgerStore, node string) (*Ledger, error) {
	if node == "" {
		return nil, errors.New("capacity: 账本需要节点标识")
	}
	return &Ledger{store: st, node: sanitizeLedgerNode(node)}, nil
}

// sanitizeLedgerNode 把节点标识归一为文件名安全片段（非 [A-Za-z0-9._-] ⇒ '_'）。
func sanitizeLedgerNode(node string) string {
	var b strings.Builder
	for _, r := range node {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "node"
	}
	return b.String()
}

// Node 返回归一后的本进程分片标识（诊断/测试用）。
func (l *Ledger) Node() string { return l.node }

// shardName / snapName 返回本进程的分片/基线文件名。
func (l *Ledger) shardName() string { return ledgerShardPrefix + l.node + ledgerShardSuffix }
func (l *Ledger) snapName() string  { return ledgerSnapPrefix + l.node }

// Add 把增量 delta 追加进**本进程分片**（delta 可正可负；0 为空操作）。
// 追加前**不做**可用额度判定（判定在 `LedgerCounter`；账本只负责如实记录）。
func (l *Ledger) Add(delta int64) error {
	if l == nil || l.store == nil {
		return errors.New("capacity: 账本未打开")
	}
	if delta == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.store.appendLine(l.shardName(), strconv.FormatInt(delta, 10)+"\n"); err != nil {
		return fmt.Errorf("capacity: 写账本分片失败: %w", err)
	}
	return nil
}

// Total 返回**全局占用**（所有基线 + 所有分片增量之和）。
//
// 发现不可解析的分片/基线 ⇒ 置降级标志并计入 `CorruptLedgerCount`，同时返回 ok=false
// （调用方必须据此 **fail-closed** 拒写，**不得**按 0 处理）。
func (l *Ledger) Total() (int64, bool) {
	if l == nil || l.store == nil {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.scanLocked()
}

func (l *Ledger) scanLocked() (int64, bool) {
	names, err := l.store.list()
	if err != nil {
		l.markCorruptLocked()
		return 0, false
	}
	var total int64
	shards := 0
	corrupt := false
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, ledgerSnapPrefix):
			n, ok := l.readNumberLocked(name)
			if !ok {
				corrupt = true
				continue
			}
			total += n
			shards++
		case strings.HasPrefix(name, ledgerShardPrefix) && strings.HasSuffix(name, ledgerShardSuffix):
			n, ok := l.readShardLocked(name)
			if !ok {
				corrupt = true
				continue
			}
			total += n
			shards++
		}
	}
	l.shards = shards
	if corrupt {
		l.markCorruptLocked()
		return 0, false
	}
	l.corrupt = false
	return total, true
}

// markCorruptLocked 置降级标志并计数（**只在状态翻转时**计数，避免每读一次就累加）。
func (l *Ledger) markCorruptLocked() {
	if !l.corrupt {
		l.corrupt = true
		ledgerCorruptShards.Add(1)
	}
}

// Degraded 报告账本是否处于降级（损坏/不可读）状态。
func (l *Ledger) Degraded() bool {
	if l == nil || l.store == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.corrupt
}

// Compact 把**本进程分片**折叠进**本进程基线**，并截断分片。
//
// 只动自己的两个文件 ⇒ 无需跨进程协调。顺序必须是「先写含新总和的基线、再截断分片」：
// 反过来（或截断失败仍留分片）会**双计**。
func (l *Ledger) Compact() error {
	if l == nil || l.store == nil {
		return errors.New("capacity: 账本未打开")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	own, ok := l.readShardLocked(l.shardName())
	if !ok {
		l.markCorruptLocked()
		return fmt.Errorf("capacity: 压缩前读取本进程分片失败: %w", ErrLedgerDegraded)
	}
	base, ok := l.readNumberLocked(l.snapName())
	if !ok {
		l.markCorruptLocked()
		return fmt.Errorf("capacity: 压缩前读取本进程基线失败: %w", ErrLedgerDegraded)
	}
	if err := l.store.writeAtomic(l.snapName(), []byte(strconv.FormatInt(base+own, 10)+"\n")); err != nil {
		return fmt.Errorf("capacity: 写账本基线失败: %w", err)
	}
	if err := l.store.truncate(l.shardName()); err != nil {
		return fmt.Errorf("capacity: 截断本进程分片失败（和会双计，需人工介入）: %w", err)
	}
	return nil
}

// Close 关闭账本（释放句柄；**不删文件**——分片是持久账本的一部分）。
func (l *Ledger) Close() error {
	if l == nil || l.store == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.store.close()
}

// LedgerShards 返回最近一次扫描发现的分片/基线文件数（诊断/测试用）。
func (l *Ledger) LedgerShards() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shards
}

// readNumberLocked 读单个十进制整数文件（缺文件 ⇒ 0,true；不可解析 ⇒ 0,false）。
func (l *Ledger) readNumberLocked(name string) (int64, bool) {
	b, ok, err := l.store.readFile(name)
	if err != nil {
		return 0, false
	}
	if !ok {
		return 0, true
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, true
	}
	n, perr := strconv.ParseInt(s, 10, 64)
	if perr != nil {
		return 0, false
	}
	return n, true
}

// readShardLocked 读分片增量之和（缺文件 ⇒ 0,true；任一行不可解析 ⇒ 0,false ⇒ 整片降级）。
func (l *Ledger) readShardLocked(name string) (int64, bool) {
	b, ok, err := l.store.readFile(name)
	if err != nil {
		return 0, false
	}
	if !ok {
		return 0, true
	}
	var total int64
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		n, perr := strconv.ParseInt(line, 10, 64)
		if perr != nil {
			return 0, false
		}
		total += n
	}
	if sc.Err() != nil {
		return 0, false
	}
	return total, true
}

// ListLedgerShards 列出目录内全部分片/基线文件名（诊断用；排序保证确定性）。
func ListLedgerShards(dir string) ([]string, error) {
	st, err := newOSStore(dir)
	if err != nil {
		return nil, err
	}
	names, err := st.list()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		if strings.HasPrefix(name, ledgerSnapPrefix) ||
			(strings.HasPrefix(name, ledgerShardPrefix) && strings.HasSuffix(name, ledgerShardSuffix)) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- LedgerCounter：Counter 适配 ----

// LedgerCounter 把 `Ledger` 适配为卷级容量 `Counter`（**共享卷**）。
//
// 与已删除的单文件计数器不同：判定基于**全局和**（Σ 所有分片）⇒ 多进程正确。
type LedgerCounter struct {
	l   *Ledger
	max int64
	mu  sync.Mutex
	// reserved 是本进程在途预留（不进账本：预留是瞬态的，崩溃后由对账兜底）。
	reserved int64
}

// NewLedgerCounter 构造共享卷卷级计数器（max<=0 表示不限）。
func NewLedgerCounter(l *Ledger, max int64) *LedgerCounter {
	return &LedgerCounter{l: l, max: max}
}

// TryAdd 在**全局和**上累计 size：超限（或账本降级）⇒ 返回错误且**不改变**账本。
func (c *LedgerCounter) TryAdd(size int64) error {
	if size <= 0 {
		return nil
	}
	if c == nil || c.l == nil {
		return ErrLedgerDegraded
	}
	total, ok := c.l.Total()
	if !ok {
		ledgerDegraded.Add(1)
		return fmt.Errorf("%w: 无法确定该卷占用，拒绝写入", ErrLedgerDegraded)
	}
	c.mu.Lock()
	reserved := c.reserved
	c.mu.Unlock()
	if c.max > 0 && total+reserved+size > c.max {
		return fmt.Errorf("%w: used=%d reserved=%d size=%d max=%d", ErrVolumeFull, total, reserved, size, c.max)
	}
	return c.l.Add(size)
}

// Release 释放（删除/覆盖/改名后）已确认占用。
func (c *LedgerCounter) Release(size int64) {
	if c == nil || c.l == nil || size <= 0 {
		return
	}
	_ = c.l.Add(-size)
}

// Used 返回**全局**已用（Σ 所有分片）。账本降级 ⇒ 返回 0 并由 `Degraded()` 报告。
func (c *LedgerCounter) Used() int64 {
	if c == nil || c.l == nil {
		return 0
	}
	total, ok := c.l.Total()
	if !ok {
		return 0
	}
	return total
}

// Capacity 返回上限（0 = 不限）。
func (c *LedgerCounter) Capacity() int64 { return c.max }

// Degraded 报告底层账本是否降级（写路径应据此 fail-closed 并告警）。
func (c *LedgerCounter) Degraded() bool {
	if c == nil || c.l == nil {
		return true
	}
	return c.l.Degraded()
}

// Flush 满足 Counter 的可选接口（账本每次 Add 即时落盘 ⇒ 空操作）。
func (c *LedgerCounter) Flush() error { return nil }

// CorruptSnapshot 保留旧接口语义：账本降级即等价于「快照不可用」（装配层据此告警）。
func (c *LedgerCounter) CorruptSnapshot() bool { return c.Degraded() }
