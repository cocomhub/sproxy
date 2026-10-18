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

	"github.com/cocomhub/sproxy/pkg/quota"
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
	// ledgerIndexName 是**分片索引**文件名：账本内全部分片名的登记表（每行一个）。
	//
	// **为何需要索引**（实测教训）：账本**不能**依赖 `syncpkg.FS.ListDir` 枚举分片 ——
	// 探针实测 `LocalFS.ListDir("meta/quota")` 对**存在且含文件**的目录返回 **0 条目** ✗
	// ⇒ 账本恒读到 0 且 `ok=true`（不降级 ⇒ **静默错** ✗）。索引只用「读/写」两个最基础能力
	// ⇒ 在任何支持读写的卷 FS 上都成立 ✓。
	ledgerIndexName = "shards"
)

// ledgerCorruptShards 是**进程级**「账本分片损坏/不可解析」计数（可观测面）。
var ledgerCorruptShards atomic.Int64

// CorruptLedgerCount 返回本进程累计的账本分片损坏次数（指标/告警面消费）。
func CorruptLedgerCount() int64 { return ledgerCorruptShards.Load() }

// ledgerDegraded 是**进程级**「账本降级 ⇒ 拒写」计数（区分「偶发损坏」与「持续拒写」）。
var ledgerDegraded atomic.Int64

// LedgerDegradedCount 返回本进程累计的「因账本降级而拒绝写入」次数。
func LedgerDegradedCount() int64 { return ledgerDegraded.Load() }

// ledgerMemFallback 是**进程级**「卷 FS 无法承载账本 ⇒ 退回内存账本」计数。
// 语义：**进程内强制仍在**（上限不会被放宽），但**跨进程可见性失效** ⇒ 必须可告警。
var ledgerMemFallback atomic.Int64

// LedgerMemFallbackCount 返回本进程累计的「账本退回内存」次数。
func LedgerMemFallbackCount() int64 { return ledgerMemFallback.Load() }

// OpenMemLedger 构造**内存账本**（降级兜底；语义与卷内账本一致，只是不跨进程）。
func OpenMemLedger(node string) (*Ledger, error) { return newLedger(newMemStore(), node) }

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
	// deleteFile 删除文件（不存在 ⇒ 空操作）。供空分片回收（ReapEmptyShards）使用。
	deleteFile(name string) error
	// close 释放句柄（幂等）。
	close() error
}

// ---- osStore：本地目录实现（测试与本地场景）----

// osStore 每次追加**即开即关**（不缓存句柄）：① 跨平台安全——Windows 上持有句柄会让
// 目录删除失败（`t.TempDir` 清理实测报 "used by another process"）；② 消除句柄泄漏面。
// 代价是每次追加一次 open，对账本这种小写入可接受（生产走 `fsStore`）。
type osStore struct {
	dir string
	mu  sync.Mutex
}

func newOSStore(dir string) (*osStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("capacity: 创建账本目录失败: %w", err)
	}
	return &osStore{dir: dir}, nil
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
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // 账本随卷
	if err != nil {
		return err
	}
	defer f.Close()
	_, werr := f.WriteString(line)
	return werr
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
	// 按**路径**截断：Windows 上对 `O_APPEND|O_WRONLY` 句柄调 `Truncate` 会
	// "Access is denied"（实测）⇒ 路径式截断跨平台可靠。
	return os.Truncate(filepath.Join(s.dir, name), 0)
}

func (s *osStore) deleteFile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(filepath.Join(s.dir, name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *osStore) close() error { return nil }

// ---- memStore：内存实现（**降级兜底**）----

// memStore 是账本的**内存**实现：当卷 FS 无法承载账本（不支持建目录/列目录，如测试替身或
// 特殊卷型）时使用。**仍然只有一套机制**（`Ledger`/`LedgerCounter` 不变），代价只是
// **失去跨进程可见性**（进程内强制仍在）⇒ 必须计入 `LedgerMemFallbackCount` 并可告警，
// 绝不静默（「限额被静默放宽」是最危险形态；而「跨进程失效」同样是必须可观测的降级）。
type memStore struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMemStore() *memStore { return &memStore{files: map[string][]byte{}} }

func (s *memStore) readFile(name string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.files[name]
	return b, ok, nil
}

func (s *memStore) appendLine(name, line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[name] = append(s.files[name], []byte(line)...)
	return nil
}

func (s *memStore) writeAtomic(name string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	s.files[name] = cp
	return nil
}

func (s *memStore) list() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.files))
	for k := range s.files {
		out = append(out, k)
	}
	return out, nil
}

func (s *memStore) truncate(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, name)
	return nil
}

func (s *memStore) deleteFile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, name)
	return nil
}

func (s *memStore) close() error { return nil }

// ---- fsStore：卷内 syncpkg.FS 实现（生产用）----

// 为何不持有 ctx：`containedctx` 静态检查禁止长生命周期结构持有 `context.Context`（它应随请求走）。
// 而账本操作发生在 `Counter` 接口之下（`TryAdd/Release/Used` 均无 ctx）⇒ 本就无法获得请求 ctx，
// 故各方法用 `context.Background()`：语义**诚实**（账本操作不可取消，由卷 FS 自身超时兜底）。
type fsStore struct {
	fs  syncpkg.FS
	dir string
	mu  sync.Mutex
	// mem 是**运行时降级兜底**：卷 FS 写失败（不支持建目录/写入语义不符/远端异常）⇒ 转内存账本，
	// **保留进程内强制**，只失去跨进程可见性 ⇒ 计入 `LedgerMemFallbackCount()` 可告警。
	// 为什么是**运行时**而非构造期判定：部分卷 FS 的 `MakeDir`/`Stat` 语义宽松（构造期看不出来），
	// 只有真写一次才暴露 ⇒ 首次失败即降级（透明、仍一套机制）。
	mem *memStore
}

// activeLocked 返回当前生效的存储（已降级 ⇒ 内存）。
func (s *fsStore) activeLocked() ledgerStore {
	if s.mem != nil {
		return s.mem
	}
	return nil
}

// degrade 是**显式降级**入口（幂等 + 计数）：供「索引写失败」等场景主动切内存兜底。
func (s *fsStore) degrade() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.degradeLocked()
}

// degradeLocked 切到内存兜底（幂等 + 计数）。
//
// **播种**（评审 A2）：降级时把卷内账本的**索引 + 全部分片/基线**复制进内存账本——
// 否则降级瞬间已记账占用从 `Total()` 视角清零（配额 fail-open ✗，探针实测：写入
// 4096 后降级 Total 掉到 0 且 ok=true）。复制后降级前后扫描同构：本进程后续增量
// append 到自己的分片（索引已含 ⇒ 可见）✓。读索引失败 ⇒ 空起点（降级即异常，
// 由 `LedgerMemFallbackCount` 告警覆盖；宁从 0 起算继续进程内强制）。
func (s *fsStore) degradeLocked() *memStore {
	if s.mem == nil {
		s.mem = newMemStore()
		s.seedMemFromVolumeLocked()
		ledgerMemFallback.Add(1)
	}
	return s.mem
}

// seedMemFromVolumeLocked 把卷内账本的**索引 + 分片/基线**复制进内存账本（降级播种）。
func (s *fsStore) seedMemFromVolumeLocked() {
	ib, ok, ierr := s.readFileLocked(ledgerIndexName)
	if ierr != nil || !ok {
		return // 索引读失败/缺失 ⇒ 空起点（降级即异常，由 LedgerMemFallbackCount 告警）
	}
	_ = s.mem.writeAtomic(ledgerIndexName, ib)
	for line := range strings.SplitSeq(string(ib), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if vb, vok, verr := s.readFileLocked(name); verr == nil && vok {
			_ = s.mem.writeAtomic(name, vb)
		}
	}
}

func newFSStore(fsys syncpkg.FS, dir string) (*fsStore, error) {
	if fsys == nil {
		return nil, errors.New("capacity: 卷内账本需要 FS")
	}
	// 建目录：**逐级**创建（卷 FS 的 MakeDir 不保证递归 ⇒ 直接建 `meta/quota` 会失败，
	// 实测报「写账本分片失败: ... cannot find the file specified」），并在建完后**复核存在性**
	// （部分实现 MakeDir 失败但不报错）⇒ 任一不成立即返回错误 ⇒ 上层退回内存账本。
	ctx := context.Background()
	cur := ""
	for seg := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		if seg == "" {
			continue
		}
		if cur == "" {
			cur = seg
		} else {
			cur += "/" + seg
		}
		if err := fsys.MakeDir(ctx, cur); err != nil {
			if _, serr := fsys.Stat(ctx, cur); serr != nil {
				return nil, fmt.Errorf("capacity: 创建卷内账本目录 %q 失败: %w", cur, err)
			}
		}
	}
	if _, serr := fsys.Stat(ctx, dir); serr != nil {
		return nil, fmt.Errorf("capacity: 卷内账本目录 %q 不可用: %w", dir, serr)
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
	s.mu.Lock()
	if a := s.activeLocked(); a != nil {
		s.mu.Unlock()
		return a.readFile(name)
	}
	s.mu.Unlock()
	rc, err := s.fs.OpenRead(context.Background(), s.path(name))
	if err != nil {
		// 区分「确定不存在」与「真错误」（评审 A3）：不存在（ok=false,nil）照旧——
		// 首次运行/索引未建的「空账本」语义；**真错误**（远端抖动/连接重置等）上抛
		// ⇒ `scanLocked` 标 corrupt ⇒ fail-closed 拒写。此前一律吞成「不存在」⇒
		// 分片被当 0、`appendLine` 把历史重写为单行 ⇒ 限额 fail-open ✗（探针实测
		// flaky FS ⇒ Total=0 ok=true）。
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if rc == nil {
		// **契约二**：不存在以 `(nil, nil)` 表达（**项目约定**，见 `pkg/volume/enclosure/region.go`
		// 的 regionAbsent 注释：「不存在的两种契约：返回 os/fs.ErrNotExist，**或**按 sync.FS
		// 契约返回 (nil, nil)」）⇒ 按「不存在」处理（首次运行 = 空账本）。
		// 既**绝不** nil 解引用 panic ✓，也**不得**判损坏 fail-closed ✗——后者会让任何按该
		// 约定实现的卷 FS 上账本恒拒写（该卷完全不可用 ✗；实测 ledger_nilreader_contract_test）。
		// 「真错误 ⇒ 不可确定 ⇒ fail-closed」由下面的 `err != nil` 分支单独保证（语义分离 ✓）。
		return nil, false, nil
	}
	defer rc.Close()
	b, rerr := io.ReadAll(rc)
	if rerr != nil {
		// 读**期间**才暴露的不存在（惰性/流式后端）同样按「不存在」处理；其余读错误
		// ⇒ 不可确定 ⇒ 上抛，由 `scanLocked` 判 corrupt（fail-closed ✓）。
		if errors.Is(rerr, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, rerr
	}
	return b, true, nil
}

func (s *fsStore) appendLine(name, line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.activeLocked(); a != nil {
		return a.appendLine(name, line)
	}
	// 卷 FS 无 O_APPEND 语义 ⇒ 读旧 + 整体原子重写。
	// 代价：每次追加一次读+写 ⇒ 仅**共享卷**走账本（本地卷零 I/O）。
	old, _, err := s.readFileLocked(name)
	if err != nil {
		return s.degradeLocked().appendLine(name, line)
	}
	buf := make([]byte, 0, len(old)+len(line))
	buf = append(buf, old...)
	buf = append(buf, line...)
	if werr := s.writeLocked(name, buf); werr != nil {
		return s.degradeLocked().appendLine(name, line)
	}
	return nil
}

// readFileLocked / writeLocked 是**不经降级判断**的裸 FS 操作（调用方已持锁）。
func (s *fsStore) readFileLocked(name string) ([]byte, bool, error) {
	ctx := context.Background()
	rc, err := s.fs.OpenRead(ctx, s.path(name))
	if err != nil {
		// 与 readFile 同口径：真错误（非「确定不存在」）上抛，调用方据此降级/拒写，
		// **不得**吞成不存在（否则 appendLine 会把历史分片重写为单行，数据丢失 ✗）。
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if rc == nil {
		return nil, false, nil // 契约二：(nil, nil) = 不存在（同 readFile）
	}
	defer rc.Close()
	b, rerr := io.ReadAll(rc)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, rerr
	}
	return b, true, nil
}

func (s *fsStore) writeLocked(name string, b []byte) error {
	ctx := context.Background()
	cur := ""
	for seg := range strings.SplitSeq(strings.Trim(s.dir, "/"), "/") {
		if seg == "" {
			continue
		}
		if cur == "" {
			cur = seg
		} else {
			cur += "/" + seg
		}
		_ = s.fs.MakeDir(ctx, cur)
	}
	return s.fs.WriteFile(ctx, syncpkg.LogicalKey{}, s.path(name), bytes.NewReader(b), int64(len(b)), 0)
}

func (s *fsStore) writeAtomic(name string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.activeLocked(); a != nil {
		return a.writeAtomic(name, b)
	}
	if err := s.writeLocked(name, b); err != nil {
		return s.degradeLocked().writeAtomic(name, b)
	}
	return nil
}

func (s *fsStore) list() ([]string, error) {
	s.mu.Lock()
	if a := s.activeLocked(); a != nil {
		s.mu.Unlock()
		return a.list()
	}
	s.mu.Unlock()
	entries, err := s.fs.ListDir(context.Background(), s.dir)
	if err != nil {
		// 目录不可列（未创建/不支持）⇒ 视作「尚无账本文件」而非降级：
		// 首次写入会真正触发降级判定。
		return nil, nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.activeLocked(); a != nil {
		return a.truncate(name)
	}
	if err := s.writeLocked(name, nil); err != nil {
		return s.degradeLocked().truncate(name)
	}
	return nil
}

func (s *fsStore) deleteFile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.activeLocked(); a != nil {
		return a.deleteFile(name)
	}
	return s.fs.Delete(context.Background(), s.path(name))
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
	// registered 记录本进程分片是否已登记进索引（进程内一次即可 ⇒ 避免每次追加都读+写索引）。
	registered bool
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
	if err := l.registerShardLocked(); err != nil {
		return fmt.Errorf("capacity: 登记账本分片失败: %w", err)
	}
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

// registerShardLocked 确保**本进程分片**已登记进索引（幂等；进程内只做一次）。
//
// 索引是账本内**唯一的枚举来源**（见 `ledgerIndexName` 说明：不依赖 ListDir 枚举能力）。
func (l *Ledger) registerShardLocked() error {
	if l.registered {
		return nil
	}
	name := l.shardName()
	b, ok, err := l.store.readFile(ledgerIndexName)
	if err != nil {
		return err
	}
	if ok {
		for line := range strings.SplitSeq(string(b), "\n") {
			if strings.TrimSpace(line) == name {
				l.registered = true
				return nil
			}
		}
	}
	buf := make([]byte, 0, len(b)+len(name)+1)
	buf = append(buf, b...)
	buf = append(buf, []byte(name+"\n")...)
	if werr := l.store.writeAtomic(ledgerIndexName, buf); werr != nil {
		// 索引写不进去（卷 FS 不支持在该路径写入等）⇒ **显式降级**（不是把错误抛给写路径 ✗）：
		// 进程内强制仍在 ✓、跨进程可见性失效 ✓ 且**可观测** ✓（`LedgerMemFallbackCount` + 告警）。
		// 这符合「跨进程 = best-effort、降级必须可观测」的既定原则（设计 §3.6.2）。
		if d, ok := l.store.(interface{ degrade() }); ok {
			d.degrade()
		}
		l.registered = true
		return nil
	}
	l.registered = true
	return nil
}

// shardNamesLocked 返回账本内全部分片与基线名（**来自索引**，不依赖 ListDir 枚举能力）。
// 基线名由分片名**派生**（`shard.<node>.log` ⇒ `snap.<node>`）⇒ 索引只需登记分片。
func (l *Ledger) shardNamesLocked() ([]string, error) {
	b, ok, err := l.store.readFile(ledgerIndexName)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 索引缺失（评审 A4）：**仅卷内持久化账本**（fsStore）且本进程分片**已登记过**
		// ⇒ 索引曾存在、现丢失 = 异常 ⇒ 按损坏处理（fail-closed 拒写），不得静默归零
		// （限额放宽 ✗，探针实测：删除索引后 Total=0 ok=true、分片仍在盘上）。
		// **内存降级**（memStore）的空索引是降级态正常形态（可被降级播种填充，也可能
		// 为空）⇒ 不得误判（否则降级卷恒拒写；TestWrapper 系实测拦截）。从未登记 ⇒
		// 首次运行，空账本合法。
		if l.registered {
			if _, isMem := l.store.(*memStore); !isMem {
				return nil, fmt.Errorf("%w: 索引文件丢失但本进程分片已登记", ErrLedgerDegraded)
			}
		}
		return nil, nil
	}
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		n := strings.TrimSpace(line)
		if n == "" {
			continue
		}
		out = append(out, n)
		if strings.HasPrefix(n, ledgerShardPrefix) && strings.HasSuffix(n, ledgerShardSuffix) {
			node := strings.TrimSuffix(strings.TrimPrefix(n, ledgerShardPrefix), ledgerShardSuffix)
			out = append(out, ledgerSnapPrefix+node)
		}
	}
	return out, nil
}

func (l *Ledger) scanLocked() (int64, bool) {
	names, err := l.shardNamesLocked()
	if err != nil {
		l.markCorruptLocked()
		return 0, false
	}
	var total int64
	corrupt := false
	for _, raw := range names {
		// **必须取 basename**：`fsStore.list()` 底层是 `syncpkg.FS.ListDir`，其 `Entry.Name`
		// 在多数卷实现里是**含目录前缀的完整键**（实测 `meta/quota/shard.seed.log`）⇒ 直接用
		// `HasPrefix("shard.")` 会**全部漏掉** ⇒ 账本恒读到 0（且 `ok=true` ⇒ 不降级、静默错 ✗）。
		name := raw
		if i := strings.LastIndex(raw, "/"); i >= 0 {
			name = raw[i+1:]
		}
		switch {
		case strings.HasPrefix(name, ledgerSnapPrefix):
			// **读取必须用原始名**（`raw`）：`fsStore.readFile` 会再拼一次目录前缀
			// ⇒ 若传 basename 则路径正确、传 raw 也对；但传「已去掉前缀的名字」会双拼 ✗。
			// 统一：**分类看 basename、读取用 raw**（两种 store 都自洽）。
			n, ok := l.readNumberLocked(raw)
			if !ok {
				corrupt = true
				continue
			}
			total += n
		case strings.HasPrefix(name, ledgerShardPrefix) && strings.HasSuffix(name, ledgerShardSuffix):
			n, ok := l.readShardLocked(raw)
			if !ok {
				corrupt = true
				continue
			}
			total += n
		}
	}
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

// reapEmptyShards 回收**空分片**（内容为空 = 已被本/他进程 Compact 截断，增量已折入其
// 基线）并同步从索引摘除。非空死亡分片**保留**（正确性优先：其增量仍计入全局和，数量有界
// 于历史进程数）。供周期对账接线（评审 S1/S3——分片/索引随重启单调增长）。
func (l *Ledger) reapEmptyShards() error {
	if l == nil || l.store == nil {
		return errors.New("capacity: 账本未打开")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	reaped := l.emptyShardNamesLocked()
	if len(reaped) == 0 {
		return nil
	}
	if ierr := l.unindexShardsLocked(reaped); ierr != nil {
		return ierr
	}
	for _, raw := range reaped {
		_ = l.store.deleteFile(raw)
	}
	return nil
}

// emptyShardNamesLocked 返回索引内内容为空的 shard 名（增量已折入基线 ⇒ 可安全回收）。
func (l *Ledger) emptyShardNamesLocked() []string {
	names, err := l.shardNamesLocked()
	if err != nil {
		return nil
	}
	var empty []string
	for _, raw := range names {
		base := raw
		if i := strings.LastIndex(raw, "/"); i >= 0 {
			base = raw[i+1:]
		}
		if !strings.HasPrefix(base, ledgerShardPrefix) || !strings.HasSuffix(base, ledgerShardSuffix) {
			continue
		}
		b, ok, rerr := l.store.readFile(raw)
		if rerr != nil || !ok {
			continue
		}
		if strings.TrimSpace(string(b)) != "" {
			continue
		}
		empty = append(empty, raw)
	}
	return empty
}

// unindexShardsLocked 从索引中摘除指定分片登记（重写索引，保留其余行）。
//
// **基线保留**（评审实测：直接摘除 shard 行会连带丢失其 snap——`shardNamesLocked` 从
// shard 行**派生** snap，shard 行没了则基线不可见 ⇒ Total 归零 ✗）。故空 shard 的基线
// **非零**时以**独立 snap 行**写入索引（`shardNamesLocked` 对非 shard 行不派生 ⇒ 无双计）；
// 基线为零（从未折叠）⇒ 直接摘除。
func (l *Ledger) unindexShardsLocked(drop []string) error {
	ib, ok, ierr := l.store.readFile(ledgerIndexName)
	if ierr != nil || !ok {
		return nil
	}
	dropSet := make(map[string]bool, len(drop))
	for _, d := range drop {
		dropSet[d] = true
	}
	var kept []string
	for line := range strings.SplitSeq(string(ib), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !dropSet[name] {
			kept = append(kept, name)
			continue
		}
		// 摘除空 shard：基线（snap.<node>）非零 ⇒ 独立登记保留（否则 Total 归零 ✗）。
		if snap := l.retainSnapOfShardLocked(name); snap != "" {
			kept = append(kept, snap)
		}
	}
	return l.store.writeAtomic(ledgerIndexName, []byte(strings.Join(kept, "\n")+"\n"))
}

// retainSnapOfShardLocked 空 shard 的基线（snap.<node>）非零时返回其名（以独立行保留，
// `shardNamesLocked` 对非 shard 行不派生 ⇒ 无双计）；基线为零/缺失 ⇒ ""（直接摘除）。
func (l *Ledger) retainSnapOfShardLocked(name string) string {
	base := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		base = name[i+1:]
	}
	if !strings.HasPrefix(base, ledgerShardPrefix) || !strings.HasSuffix(base, ledgerShardSuffix) {
		return ""
	}
	node := strings.TrimSuffix(strings.TrimPrefix(base, ledgerShardPrefix), ledgerShardSuffix)
	snap := ledgerSnapPrefix + node
	nb, nok, nerr := l.store.readFile(snap)
	if nerr != nil || !nok || strings.TrimSpace(string(nb)) == "0" {
		return ""
	}
	return snap
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
	// pool 可选：**镜像**本进程视图（既有契约「与路由/指标/对账同源」——`pool.Usage()` 被
	// 卷路由、`sproxy_storage_usage_bytes` 指标、对账矩阵消费）。账本是**跨进程真值**，
	// pool 是**本进程视图**：强制与查询都以账本为准，pool 仅作下游读取面 ⇒ 二者可能不等
	// （其他进程写入时 pool < 账本），这是**正确**的（pool 不再是权威）。
	pool *quota.Pool
	// initErr 非 nil = 账本**构造/上限持久化失败** ⇒ 一切写入 fail-closed，
	// 绝不静默无额度（「限额被静默放宽」是本项目认定的最危险形态）。
	initErr error
	// mu 串行化进程内「检查-再追加」（TryAdd）与 Release 钳制：`l.Total()`（锁 l.mu）
	// 与 `l.Add()`（再锁 l.mu）之间必须原子，否则并发 TryAdd 都读同一 total 双过双入账
	// 可超限（评审 A1，探针实测 552/3000 次超限）。锁序恒 c.mu → l.mu（无反向嵌套）。
	mu sync.Mutex
}

// NewLedgerCounter 构造共享卷卷级计数器（max<=0 表示不限）。
func NewLedgerCounter(l *Ledger, max int64) *LedgerCounter {
	return &LedgerCounter{l: l, max: max}
}

// WithPool 让计数器同时**镜像**到卷容量池（保持既有「与路由/指标/对账同源」契约）。
// nil ⇒ 不镜像（测试/无池场景）。
func (c *LedgerCounter) WithPool(p *quota.Pool) *LedgerCounter {
	if c != nil {
		c.pool = p
	}
	return c
}

// TryAdd 在**全局和**上累计 size：超限（或账本降级）⇒ 返回错误且**不改变**账本。
func (c *LedgerCounter) TryAdd(size int64) error {
	if size <= 0 {
		return nil
	}
	if c == nil {
		return ErrLedgerDegraded
	}
	if c.initErr != nil {
		ledgerDegraded.Add(1)
		return fmt.Errorf("%w: 账本不可用（%v），拒绝写入", ErrLedgerDegraded, c.initErr)
	}
	if c.l == nil {
		return ErrLedgerDegraded
	}
	// 进程内「检查-再追加」原子段（c.mu）：并发 TryAdd 若都读同一 total 再各自 Add，
	// 双过双入账 ⇒ 超限（评审 A1，探针实测 552/3000 次超限、最大 total=120>100）。
	c.mu.Lock()
	defer c.mu.Unlock()
	total, ok := c.l.Total()
	if !ok {
		ledgerDegraded.Add(1)
		return fmt.Errorf("%w: 无法确定该卷占用，拒绝写入", ErrLedgerDegraded)
	}
	if c.max > 0 && total+size > c.max {
		return fmt.Errorf("%w: used=%d size=%d max=%d", ErrVolumeFull, total, size, c.max)
	}
	// **父链上限校验（必须）**：`c.max` 只是**本卷**上限；卷池可能是**子池**（封装卷继承源卷、
	// 用户卷挂在全局池下）⇒ 祖先上限同样必须拦住 ✗。旧实现靠 `pool.TryReserve`（`reserveUp`
	// 沿父链逐层校验）覆盖了这一点；仅比对 `c.max` 会**漏掉父链** ⇒ 多计（实测：封装卷无自身
	// 上限、父池 10B，仍把 347B 的 meta 计了进去 ⇒ 353 vs 6 ✗）。
	var poolRes *quota.Reservation
	if c.pool != nil {
		r, rerr := c.pool.TryReserve(size)
		if rerr != nil {
			return fmt.Errorf("%w: %v", ErrVolumeFull, rerr)
		}
		poolRes = r
	}
	if err := c.l.Add(size); err != nil {
		if poolRes != nil {
			poolRes.Release()
		}
		return err
	}
	if poolRes != nil {
		poolRes.Commit(size)
	}
	return nil
}

// mirrorToPool 把增量镜像到卷容量池（下游读取面）。
//
// 注意：`TryAdd` **不用**它（那条路径必须走 `pool.TryReserve` 以覆盖**父链上限校验** ✗），
// 只有 `Release`/`CorrectTo`（减少/修正，无「是否放行」语义）才用它。
func (c *LedgerCounter) mirrorToPool(delta int64) {
	if c == nil || c.pool == nil || delta == 0 {
		return
	}
	if delta > 0 {
		c.pool.Adjust(0, delta)
		return
	}
	c.pool.Adjust(-delta, 0)
}

// Release 释放（删除/覆盖/改名后）已确认占用。
func (c *LedgerCounter) Release(size int64) {
	if c == nil || c.l == nil || size <= 0 {
		return
	}
	// 防负漂移（评审 A7）：删除**从未入账**的字节（建卷前已存在 / 绕过包装的写入）会把
	// 账本总和拖负 ⇒ TryAdd 以负基线放行更多（fail-open ✗）。钳制：当前总和不足时
	// 只释放到 0——账本偏高（fail-closed 方向），由对账收敛。
	c.mu.Lock()
	defer c.mu.Unlock()
	total, ok := c.l.Total()
	if !ok {
		return // 无法确定 ⇒ 不释放（保持账本现值，不误放行）
	}
	if size > total {
		size = total
	}
	if size <= 0 {
		return
	}
	if err := c.l.Add(-size); err != nil {
		return
	}
	c.mirrorToPool(-size)
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
	if c == nil || c.l == nil || c.initErr != nil {
		return true
	}
	return c.l.Degraded()
}

// Flush 满足 Counter 的可选接口（账本每次 Add 即时落盘 ⇒ 空操作）。
func (c *LedgerCounter) Flush() error { return nil }

// Compact 折叠**本进程分片**进本进程基线并截断分片（周期对账接线——控制本进程分片
// 单调增长，评审 S1/S3）。
func (c *LedgerCounter) Compact() error {
	if c == nil || c.l == nil {
		return nil
	}
	return c.l.Compact()
}

// ReapEmptyShards 回收**空分片**并摘除索引登记（防历史进程分片文件堆积，评审 S1/S3）。
// 空分片 = 已被各进程 Compact 截断（增量已折入基线）⇒ 删除安全；非空死亡分片保留。
func (c *LedgerCounter) ReapEmptyShards() error {
	if c == nil || c.l == nil {
		return nil
	}
	return c.l.reapEmptyShards()
}
