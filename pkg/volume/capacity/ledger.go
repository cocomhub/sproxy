// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ledger.go —— **共享卷上的跨进程配额账本**（分片日志，无锁）。
//
// 设计文档：`docs/designs/2026-10-17-quota-ledger-persistence.md`。
//
// # 要解决的具体缺陷
//
// 多个 sproxy 进程共享**同一外部/集群卷**时，各进程的内存账本（`quota.Pool.committed`）
// 互不可见 ⇒ 各自按「自己看到的用量」判定 ⇒ 可同时通过 ⇒ **占用总和超限**（"卷打满"）。
// 本地卷不受影响（各节点自有磁盘 ⇒ 各自记账本就正确），故本机制**只用于共享卷**。
//
// # 为什么是「分片日志」而不是「文件锁 + 读-改-写」
//
// 共享卷（NFS/S3/WebDAV/baidupcs）上的跨进程互斥不可靠（POSIX 锁跨节点语义不一致），
// 引入分布式锁会同时带来**新依赖**与**可用性下降**。改为**每个进程只写自己的分片**
// （单写者 ⇒ 无需锁），读时求和 ⇒ 无协调、无丢失更新、崩溃安全。
//
// # 目录布局（`<卷内键前缀>/`，由调用方给定）
//
//	shard.<node>.log   本进程 append-only 增量：每行一个带符号十进制整数（人类可读、可 tail）
//	snap.<node>        本进程压缩后的基线：单个十进制整数（原子 tmp+rename 写入）
//
// # 权威性（重要）
//
// **对账（磁盘实况）是最终权威**，账本只用于在途期间的快速判定：崩溃只丢「未 flush 的增量」
// ⇒ 方向是**少计**（可能超限，不会误拒），由对账周期约束窗口。
// 跨进程的「检查-再追加」本身非原子（无锁设计的必然代价）⇒ 残余**有界**超限窗口，
// 由对账 + 卷级硬上限共同收敛（见设计文档 §7 R1/R3）。
//
// # 损坏语义：**fail-closed**（不 fail-open）
//
// 分片不可解析 ⇒ 无法确定真实占用 ⇒ **拒绝新写入**并计数/告警，直到调用方压缩或对账清理。
// 理由：「限额被静默放宽」是本项目认定的**最危险形态**（同 `CorruptSnapshotCount` 的取向）。

// ErrLedgerDegraded 表示账本损坏/不可读 ⇒ 无法确定占用 ⇒ **fail-closed** 拒绝新写入。
var ErrLedgerDegraded = errors.New("quota ledger degraded")

// ledgerShardPrefix / ledgerSnapPrefix 是分片与基线的文件名前缀（其余部分为节点标识）。
const (
	ledgerShardPrefix = "shard."
	ledgerSnapPrefix  = "snap."
	ledgerShardSuffix = ".log"
)

// ledgerCorruptShards 是**进程级**「账本分片损坏/不可解析」计数（可观测面，与
// `CorruptSnapshotCount` 同型）：限额生效状态必须可观测，损坏即等价「限额被放宽一轮」。
var ledgerCorruptShards atomic.Int64

// CorruptLedgerCount 返回本进程累计的账本分片损坏次数（指标/告警面消费）。
func CorruptLedgerCount() int64 { return ledgerCorruptShards.Load() }

// ledgerDegraded 是**进程级**「账本降级 ⇒ 拒写」计数（用于告警区分「偶发损坏」与「持续拒写」）。
var ledgerDegraded atomic.Int64

// LedgerDegradedCount 返回本进程累计的「因账本降级而拒绝写入」次数。
func LedgerDegradedCount() int64 { return ledgerDegraded.Load() }

// Ledger 是某「配额键」（卷级容量，或某用户/桶级配额）在**共享卷**上的跨进程账本。
//
// 零值不可用；用 `OpenLedger` 构造。**并发安全**（内部互斥保护本进程的追加句柄与缓存）。
type Ledger struct {
	mu   sync.Mutex
	dir  string
	node string
	// append 句柄（O_APPEND ⇒ 单写者追加无需外部锁）。
	f *os.File
	// corrupt 记录本次扫描是否发现不可解析的分片（⇒ 降级）。
	corrupt bool
	// shards 是最近一次扫描发现的分片文件数（诊断/测试用）。
	shards int
}

// OpenLedger 打开（必要时创建）dir 上的账本，并以 node 作为本进程的分片标识。
//
// node 必须**稳定且唯一**（同一进程重启可复用；不同进程必须不同）——调用方负责
// （通常为「主机名 + 进程实例标识」）。dir 为空 ⇒ 返回错误（调用方应据此退回纯内存路径）。
func OpenLedger(dir, node string) (*Ledger, error) {
	if dir == "" || node == "" {
		return nil, fmt.Errorf("capacity: 账本需要目录与节点标识（dir=%q node=%q）", dir, node)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("capacity: 创建账本目录失败: %w", err)
	}
	// 分片名做**文件名安全**归一（node 可能含 `/`、`:` 等，直接拼会跨目录或非法）。
	safe := sanitizeLedgerNode(node)
	p := filepath.Join(dir, ledgerShardPrefix+safe+ledgerShardSuffix)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // 账本按卷内键空间存放，权限随卷
	if err != nil {
		return nil, fmt.Errorf("capacity: 打开账本分片失败: %w", err)
	}
	return &Ledger{dir: dir, node: safe, f: f}, nil
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

// Add 把增量 delta 追加进**本进程分片**（delta 可正可负；0 为空操作）。
//
// 追加前**不做**可用额度判定（判定在 `LedgerCounter`/调用方；账本只负责如实记录）。
func (l *Ledger) Add(delta int64) error {
	if l == nil || l.f == nil {
		return errors.New("capacity: 账本未打开")
	}
	if delta == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// 单行一增量：人类可读、可 tail、可人工修复（损坏时整片降级而非半信）。
	if _, err := fmt.Fprintf(l.f, "%d\n", delta); err != nil {
		return fmt.Errorf("capacity: 写账本分片失败: %w", err)
	}
	return nil
}

// Total 返回**全局占用**（本进程基线 + 所有分片增量之和）。
//
// 发现不可解析的分片 ⇒ 置降级标志并计入 `CorruptLedgerCount`，同时返回 ok=false
// （调用方必须据此 **fail-closed** 拒写，不得按 0 处理）。
func (l *Ledger) Total() (int64, bool) {
	if l == nil || l.f == nil {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.scanLocked()
}

// scanLocked 扫描目录求和（持锁调用）。返回 ok=false 表示发现损坏分片（已置降级）。
func (l *Ledger) scanLocked() (int64, bool) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		l.markCorruptLocked()
		return 0, false
	}
	var total int64
	shards := 0
	corrupt := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasPrefix(name, ledgerSnapPrefix):
			n, ok := readLedgerNumber(filepath.Join(l.dir, name))
			if !ok {
				corrupt = true
				continue
			}
			total += n
			shards++
		case strings.HasPrefix(name, ledgerShardPrefix) && strings.HasSuffix(name, ledgerShardSuffix):
			n, ok := readLedgerShard(filepath.Join(l.dir, name))
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
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.corrupt
}

// Compact 把**本进程分片**折叠进**本进程基线**（snap.<node>），并截断分片。
//
// 只动自己的两个文件 ⇒ 无需跨进程协调。压缩期间其他进程读到的和**不变**
// （写基线成功后才截断分片；截断失败 ⇒ 下次压缩重试，和仍正确——基线含旧值 + 分片重放会双计，
// 故顺序必须是「先写含新总和的基线、再截断分片」，且失败时**不**截断）。
func (l *Ledger) Compact() error {
	if l == nil || l.f == nil {
		return errors.New("capacity: 账本未打开")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	own, ok := readLedgerShard(filepath.Join(l.dir, ledgerShardPrefix+l.node+ledgerShardSuffix))
	if !ok {
		l.markCorruptLocked()
		return fmt.Errorf("capacity: 压缩前读取本进程分片失败: %w", ErrLedgerDegraded)
	}
	base, ok := readLedgerNumber(filepath.Join(l.dir, ledgerSnapPrefix+l.node))
	if !ok {
		l.markCorruptLocked()
		return fmt.Errorf("capacity: 压缩前读取本进程基线失败: %w", ErrLedgerDegraded)
	}
	if err := writeLedgerNumber(filepath.Join(l.dir, ledgerSnapPrefix+l.node), base+own); err != nil {
		return err
	}
	// 基线已含 own ⇒ 截断分片（失败则下次压缩重试；**不能**在失败时留下已计入 own 的分片 ⇒
	// 那会双计）。
	//
	// 为什么是「关闭 → 按路径截断 → 重开」而不是直接 `l.f.Truncate(0)`：Windows 上对
	// `O_APPEND|O_WRONLY` 句柄调 Truncate 会 "Access is denied"（实测）⇒ 路径式截断是
	// 跨平台可靠的形态（与 `writeCounterFile` 的 tmp+rename 同属「不依赖句柄语义」的取向）。
	shardPath := filepath.Join(l.dir, ledgerShardPrefix+l.node+ledgerShardSuffix)
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("capacity: 关闭分片句柄失败: %w", err)
	}
	l.f = nil
	if err := os.Truncate(shardPath, 0); err != nil {
		return fmt.Errorf("capacity: 截断本进程分片失败（和会双计，需人工介入）: %w", err)
	}
	f, err := os.OpenFile(shardPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // 账本随卷
	if err != nil {
		return fmt.Errorf("capacity: 重开本进程分片失败（本进程后续写入不可用）: %w", err)
	}
	l.f = f
	return nil
}

// Close 关闭本进程分片句柄（进程退出/测试收尾用；不删文件——分片是持久账本的一部分）。
func (l *Ledger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.f.Close()
	l.f = nil
	return err
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

// readLedgerNumber 读单个十进制整数文件（缺文件 ⇒ 0,true；不可解析 ⇒ 0,false）。
func readLedgerNumber(path string) (int64, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // 路径由调用方给定的账本目录拼接
	if err != nil {
		if os.IsNotExist(err) {
			return 0, true
		}
		return 0, false
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

// readLedgerShard 读分片增量之和（缺文件 ⇒ 0,true；任一行不可解析 ⇒ 0,false ⇒ 整片降级）。
func readLedgerShard(path string) (int64, bool) {
	f, err := os.Open(path) //nolint:gosec // 同上
	if err != nil {
		if os.IsNotExist(err) {
			return 0, true
		}
		return 0, false
	}
	defer f.Close()
	var total int64
	sc := bufio.NewScanner(f)
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

// writeLedgerNumber 原子写单个十进制整数文件（tmp + rename + 目录 fsync，与
// `writeCounterFile` 同口径——同一包内保持两种持久化形态的语义一致）。
func writeLedgerNumber(path string, n int64) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)+"\n"), 0o644); err != nil { //nolint:gosec // 账本随卷
		return fmt.Errorf("capacity: 写账本基线失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("capacity: 提交账本基线失败: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// LedgerCounter 把 `Ledger` 适配为 `Counter`（**共享卷**的卷级容量记账）。
//
// 与 `VolumeCapacityCounter`/`PoolCounter` 的差异：后两者是「单文件快照 + 进程内读改写」
// （多进程共享同一卷时会丢失更新），本类型是「分片日志求和」（多进程正确）。
type LedgerCounter struct {
	l   *Ledger
	max int64
	mu  sync.Mutex
	// used 是本进程**已知**的全局和缓存（用于 TryAdd 的检查；每次 TryAdd/Used 重新求和）。
	used int64
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
	c.used = total
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

// Used 返回**全局**已用（Σ 所有分片）。账本降级 ⇒ 返回 0 并由 `Degraded()` 报告
// （调用方须先查 `Degraded()`；本方法不返回错误以保持 `Counter` 接口最小）。
func (c *LedgerCounter) Used() int64 {
	if c == nil || c.l == nil {
		return 0
	}
	total, ok := c.l.Total()
	if !ok {
		return 0
	}
	c.mu.Lock()
	c.used = total
	c.mu.Unlock()
	return total
}

// Capacity 返回上限（0 = 不限）。
func (c *LedgerCounter) Capacity() int64 { return c.max }

// Degraded 报告底层账本是否降级（装配层/写路径应据此 fail-closed 并告警）。
func (c *LedgerCounter) Degraded() bool {
	if c == nil || c.l == nil {
		return true
	}
	return c.l.Degraded()
}

// ListLedgerShards 返回目录内全部分片/基线文件名（诊断用；排序保证确定性）。
func ListLedgerShards(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ledgerSnapPrefix) ||
			(strings.HasPrefix(name, ledgerShardPrefix) && strings.HasSuffix(name, ledgerShardSuffix)) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
