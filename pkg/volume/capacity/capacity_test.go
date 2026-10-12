// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// capacity_test.go 钉住 C2 卷级计数记账：写入累计 / 删除释放 / 超限拒绝 / 持久化。
//
// 用户定案（2026-09-18）：卷级计数记账——外部卷写入累计 + 删除释放，超限拒绝；
// 本系统可用限额（UserVolume.Capacity）强制生效。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ---- VolumeCapacityCounter：写入累计 / 释放 / 超限拒绝 / 持久化 ----

// TestCounter_TryAdd_RespectsCapacity 钉住超限拒绝：used + size > capacity → 错误。
func TestCounter_TryAdd_RespectsCapacity(t *testing.T) {
	t.Parallel()
	c := NewCounter(100, "")
	if err := c.TryAdd(60); err != nil {
		t.Fatalf("TryAdd(60) = %v, want nil", err)
	}
	if got := c.Used(); got != 60 {
		t.Fatalf("Used = %d, want 60", got)
	}
	if err := c.TryAdd(50); err == nil {
		t.Fatal("TryAdd(50) 超限应返回错误（60+50>100）")
	}
	if got := c.Used(); got != 60 {
		t.Fatalf("TryAdd 失败后 Used = %d, want 60（不变）", got)
	}
	c.Release(60)
	if got := c.Used(); got != 0 {
		t.Fatalf("Release 后 Used = %d, want 0", got)
	}
}

// TestCounter_ZeroCapacity_Unlimited 钉住 capacity=0 语义：不限（只记账不拒绝）。
func TestCounter_ZeroCapacity_Unlimited(t *testing.T) {
	t.Parallel()
	c := NewCounter(0, "")
	if err := c.TryAdd(1 << 40); err != nil {
		t.Fatalf("capacity=0 应不限制: %v", err)
	}
}

// TestCounter_Persist_Roundtrip 钉住持久化：TryAdd 后 Save，Load 恢复 used。
func TestCounter_Persist_Roundtrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cap.json")
	c := NewCounter(100, path)
	if err := c.TryAdd(40); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	c2, err := Load(path, 100)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c2.Used(); got != 40 {
		t.Fatalf("Load 后 Used = %d, want 40", got)
	}
}

// ---- CapacityFS 装饰器：WriteFile 累计 / Delete 释放 / 超限拒绝 ----

// recordingFS 是记录 WriteFile/Delete 调用的 fake sync.FS（内存 size 表，支持覆盖写）。
type recordingFS struct {
	mu     sync.Mutex
	sizes  map[string]int64
	writes atomic.Int64
	dels   atomic.Int64
}

func newRecordingFS() *recordingFS { return &recordingFS{sizes: map[string]int64{}} }

func (r *recordingFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (r *recordingFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sizes[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, Size: s}, nil
	}
	return nil, nil
}
func (r *recordingFS) OpenRead(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (r *recordingFS) WriteFile(_ context.Context, p string, _ io.Reader, size, _ int64) error {
	r.writes.Add(1)
	r.mu.Lock()
	r.sizes[p] = size
	r.mu.Unlock()
	return nil
}
func (r *recordingFS) Rename(_ context.Context, from, to string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sizes[from]; ok {
		r.sizes[to] = s
		delete(r.sizes, from)
	}
	return nil
}
func (r *recordingFS) Delete(_ context.Context, p string) error {
	r.dels.Add(1)
	r.mu.Lock()
	delete(r.sizes, p)
	r.mu.Unlock()
	return nil
}
func (r *recordingFS) MakeDir(context.Context, string) error { return nil }

// TestCapacityFS_WriteAddsAndDeleteReleases 钉住装饰器：新建 WriteFile 累计 size，Delete 释放。
func TestCapacityFS_WriteAddsAndDeleteReleases(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(100, "")
	fs := Wrap(inner, c)

	if err := fs.WriteFile(context.Background(), "a.txt", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := c.Used(); got != 30 {
		t.Fatalf("WriteFile 后 Used = %d, want 30", got)
	}
	if err := fs.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := c.Used(); got != 0 {
		t.Fatalf("Delete 后 Used = %d, want 0", got)
	}
	if inner.writes.Load() != 1 || inner.dels.Load() != 1 {
		t.Fatalf("inner 调用: writes=%d dels=%d, want 1/1", inner.writes.Load(), inner.dels.Load())
	}
}

// TestCapacityFS_WriteExceedsCapacity_Rejected 钉住装饰器超限拒绝：
// capacity 满时 WriteFile 返回错误（不触达 inner）。
func TestCapacityFS_WriteExceedsCapacity_Rejected(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(50, "")
	fs := Wrap(inner, c)

	if err := fs.WriteFile(context.Background(), "a.txt", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile(30): %v", err)
	}
	if err := fs.WriteFile(context.Background(), "b.txt", nil, 30, 0); err == nil {
		t.Fatal("WriteFile(30) 超限（60>50）应返回错误")
	}
	if got := c.Used(); got != 30 {
		t.Fatalf("拒绝后 Used = %d, want 30（不变）", got)
	}
	if inner.writes.Load() != 1 {
		t.Fatalf("inner writes = %d, want 1（超限不触达）", inner.writes.Load())
	}
}

// TestCapacityFS_OverwriteAdjustsDelta 钉住覆盖写差分（2026-10-10）：覆盖小文件释放差额，
// 变大时只按净增收取；超净增部分被拒绝。
func TestCapacityFS_OverwriteAdjustsDelta(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(50, "")
	fs := Wrap(inner, c)
	ctx := context.Background()

	if err := fs.WriteFile(ctx, "x.bin", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile(30): %v", err)
	}
	if err := fs.WriteFile(ctx, "x.bin", nil, 10, 0); err != nil {
		t.Fatalf("覆盖写小文件: %v", err)
	}
	if got := c.Used(); got != 10 {
		t.Fatalf("覆盖写变小后 Used = %d, want 10", got)
	}
	// 净增 50（10 → 60）超限（10+50>50）→ 拒绝，且不触达 inner。
	writesBefore := inner.writes.Load()
	if err := fs.WriteFile(ctx, "x.bin", nil, 60, 0); err == nil {
		t.Fatal("覆盖写净增超限应拒绝")
	}
	if got := c.Used(); got != 10 {
		t.Fatalf("拒绝后 Used = %d, want 10（不变）", got)
	}
	if inner.writes.Load() != writesBefore {
		t.Fatal("超限不应触达 inner WriteFile")
	}
}

// TestCapacityFS_RenameFreesTarget 钉住改名/移动释放目标旧字节（源已在账上）。
func TestCapacityFS_RenameFreesTarget(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(50, "")
	fs := Wrap(inner, c)
	ctx := context.Background()

	if err := fs.WriteFile(ctx, "src.bin", nil, 20, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, "dst.bin", nil, 15, 0); err != nil {
		t.Fatal(err)
	}
	if got := c.Used(); got != 35 {
		t.Fatalf("Used = %d, want 35", got)
	}
	if err := fs.Rename(ctx, "src.bin", "dst.bin"); err != nil { // 覆盖 dst（旧 15 释放）
		t.Fatal(err)
	}
	if got := c.Used(); got != 20 {
		t.Fatalf("Rename 后 Used = %d, want 20（目标旧 15 释放）", got)
	}
}

// measuringFS 记录 WriteFile 实际读到的字节数（用于 size<=0 未知长度的实测记账断言）。
type measuringFS struct {
	mu    sync.Mutex
	sizes map[string]int64
}

func (m *measuringFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (m *measuringFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sizes[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, Size: s}, nil
	}
	return nil, nil
}
func (m *measuringFS) OpenRead(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (m *measuringFS) WriteFile(_ context.Context, p string, r io.Reader, size, _ int64) error {
	n := size
	if r != nil {
		w, _ := io.Copy(io.Discard, r)
		n = w
	}
	m.mu.Lock()
	m.sizes[p] = n
	m.mu.Unlock()
	return nil
}
func (m *measuringFS) Rename(context.Context, string, string) error { return nil }
func (m *measuringFS) Delete(context.Context, string) error         { return nil }
func (m *measuringFS) MakeDir(context.Context, string) error        { return nil }

// TestCapacityFS_UnknownSizeUsesMeasured 对抗评审：size<=0（未知长度，如 ContentLength=-1）
// 此前会反向记账（delta<0 → Release 抹掉他人占用）；现按实测字节入账。
func TestCapacityFS_UnknownSizeUsesMeasured(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	fs := Wrap(&measuringFS{sizes: map[string]int64{}}, NewPoolCounter(pool))
	content := strings.Repeat("x", 30)
	if err := fs.WriteFile(context.Background(), "a.bin", strings.NewReader(content), -1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := pool.Usage(); got != 30 {
		t.Fatalf("未知长度应按实测入账 30, got %d", got)
	}
	// 已知长度超限仍 fail-closed。
	if err := fs.WriteFile(context.Background(), "b.bin", nil, 80, 0); err == nil {
		t.Fatal("30+80 超限应拒绝")
	}
}

// wiaMeasuringFS 支持 WriteIfAbsent 的 measuringFS（未知长度实测 + 写后回滚用）。
type wiaMeasuringFS struct {
	measuringFS
	deleted []string
}

func (m *wiaMeasuringFS) WriteIfAbsent(_ context.Context, p string, r io.Reader, size, _ int64) (bool, error) {
	m.mu.Lock()
	_, exists := m.sizes[p]
	m.mu.Unlock()
	if exists {
		return false, nil
	}
	n := size
	if r != nil {
		w, _ := io.Copy(io.Discard, r)
		n = w
	}
	m.mu.Lock()
	m.sizes[p] = n
	m.mu.Unlock()
	return true, nil
}
func (m *wiaMeasuringFS) Delete(_ context.Context, p string) error {
	m.mu.Lock()
	delete(m.sizes, p)
	m.deleted = append(m.deleted, p)
	m.mu.Unlock()
	return nil
}

// TestCapacityFS_WriteIfAbsent_MeasuredAndRollback 第 4 轮对抗评审 P1：
// ① WriteIfAbsent 未知长度须按实测记账（原按声明 size，size<=0 完全漏记）；
// ② 未知长度写后超限须回滚（删除刚写对象）并返回错误，恢复不变量。
func TestCapacityFS_WriteIfAbsent_MeasuredAndRollback(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	inner := &wiaMeasuringFS{sizes: map[string]int64{}}
	fs := Wrap(inner, NewPoolCounter(pool))
	content := strings.Repeat("x", 30)
	ok, err := fs.WriteIfAbsent(context.Background(), "a.bin", strings.NewReader(content), -1, 0)
	if err != nil || !ok {
		t.Fatalf("WriteIfAbsent: ok=%v err=%v", ok, err)
	}
	if got := pool.Usage(); got != 30 {
		t.Fatalf("未知长度应按实测入账 30, got %d", got)
	}
	// 再来一个 80B 未知长度：写后 30+80>100 → 回滚（删除 b.bin）且报错。
	_, err = fs.WriteIfAbsent(context.Background(), "b.bin", strings.NewReader(strings.Repeat("y", 80)), -1, 0)
	if err == nil {
		t.Fatal("未知长度写后超限应返回错误（fail-closed）")
	}
	inner.mu.Lock()
	_, existed := inner.sizes["b.bin"]
	inner.mu.Unlock()
	if existed {
		t.Fatal("超限回滚应删除刚写对象 b.bin")
	}
	if got := pool.Usage(); got != 30 {
		t.Fatalf("回滚后 Usage=%d want 30", got)
	}
}

// TestLoad_CorruptSnapshotFlagged P2：损坏/版本不识的快照须被**标记**（此前静默重置
// used=0，重启后可超额写入且无任何信号）。
func TestLoad_CorruptSnapshotFlagged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.capacity")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !c.CorruptSnapshot() {
		t.Fatal("损坏快照应被标记")
	}
	if c.Used() != 0 {
		t.Fatalf("损坏快照 used=%d want 0", c.Used())
	}
	// 正常快照：不标记、used 恢复。
	ok := NewCounter(100, path)
	if aerr := ok.TryAdd(10); aerr != nil {
		t.Fatal(aerr)
	}
	if serr := ok.Save(); serr != nil {
		t.Fatal(serr)
	}
	c2, err := Load(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if c2.CorruptSnapshot() {
		t.Fatal("正常快照不应标记")
	}
	if c2.Used() != 10 {
		t.Fatalf("正常快照 used=%d want 10", c2.Used())
	}
	// 版本不识：标记。
	if werr := os.WriteFile(path, []byte(`{"version":99,"used":5}`), 0o600); werr != nil {
		t.Fatal(werr)
	}
	c3, err := Load(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !c3.CorruptSnapshot() {
		t.Fatal("版本不识应标记")
	}
}

// TestNewPoolCounterPersistent_CorruptFlagged P2：配置卷池计数器损坏快照同样标记。
func TestNewPoolCounterPersistent_CorruptFlagged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v.capacity")
	if err := os.WriteFile(path, []byte("boom"), 0o600); err != nil {
		t.Fatal(err)
	}
	pc, err := NewPoolCounterPersistent(quota.NewPool(1000), path)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.CorruptSnapshot() {
		t.Fatal("池计数器损坏快照应标记")
	}
	if pc.pool.Usage() != 0 {
		t.Fatalf("损坏快照 Usage=%d want 0", pc.pool.Usage())
	}
}

// ---- 评审 F2：写失败后的记账复核（"写错误" ≠ "未落盘"） ----

// commitThenErrFS 模拟**远端后端**：写盘（对象已提交）后返回错误（响应超时/连接中断，
// 无原子 rename——对象实际已落盘）。Stat 返回真实状态。
type commitThenErrFS struct {
	mu    sync.Mutex
	sizes map[string]int64
}

func (f *commitThenErrFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (f *commitThenErrFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sizes[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, Size: s}, nil
	}
	return nil, nil
}
func (f *commitThenErrFS) OpenRead(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (f *commitThenErrFS) WriteFile(_ context.Context, p string, r io.Reader, size, _ int64) error {
	n := size
	if r != nil {
		w, _ := io.Copy(io.Discard, r)
		n = w
	}
	f.mu.Lock()
	f.sizes[p] = n
	f.mu.Unlock()
	return errors.New("remote: 写响应丢失（对象可能已提交）")
}
func (f *commitThenErrFS) Rename(context.Context, string, string) error { return nil }
func (f *commitThenErrFS) Delete(_ context.Context, p string) error {
	f.mu.Lock()
	delete(f.sizes, p)
	f.mu.Unlock()
	return nil
}
func (f *commitThenErrFS) MakeDir(context.Context, string) error { return nil }

// statErrFS 同 commitThenErrFS，但 Stat 失败（无法复核）。
type statErrFS struct{ commitThenErrFS }

func (f *statErrFS) Stat(context.Context, string) (*syncpkg.Entry, error) {
	return nil, errors.New("remote: stat 失败")
}

// noWriteErrFS 写失败且**未落盘**（本地原子写语义：旧对象原样保留）。
type noWriteErrFS struct{ sizes map[string]int64 }

func (f *noWriteErrFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (f *noWriteErrFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	if s, ok := f.sizes[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, Size: s}, nil
	}
	return nil, nil
}
func (f *noWriteErrFS) OpenRead(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (f *noWriteErrFS) WriteFile(context.Context, string, io.Reader, int64, int64) error {
	return errors.New("local: 写失败（对象未变）")
}
func (f *noWriteErrFS) Rename(context.Context, string, string) error { return nil }
func (f *noWriteErrFS) Delete(_ context.Context, p string) error {
	delete(f.sizes, p)
	return nil
}
func (f *noWriteErrFS) MakeDir(context.Context, string) error { return nil }

// TestCapacityFS_WriteFailureKeepsCommittedAccounting 回归（评审 F2）：写返回错误但对象
// **已提交**（远端无原子写）时，占用必须记为实测值——此前一律 Release(reserved) 假定未落盘
// ⇒ used 永久偏低（限额可穿透 ~单文件大小且不重启不自愈）。
//
// 变异：把错误路径改回「Release(reserved)」→ Usage=0 → 红。
func TestCapacityFS_WriteFailureKeepsCommittedAccounting(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	fs := Wrap(&commitThenErrFS{sizes: map[string]int64{}}, NewPoolCounter(pool))
	if err := fs.WriteFile(context.Background(), "a.bin", strings.NewReader(strings.Repeat("x", 40)), 40, 0); err == nil {
		t.Fatal("应返回写错误")
	}
	if got := pool.Usage(); got != 40 {
		t.Fatalf("写错误但对象已提交 → 占用应记 40（清理回滚=限额穿透），got %d", got)
	}
}

// TestCapacityFS_WriteFailureNotPersistedRollsBack：写失败且**确实未落盘**（本地原子写：
// 旧对象原样保留）→ 预留回滚到旧口径（不泄漏占用）。
func TestCapacityFS_WriteFailureNotPersistedRollsBack(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	inner := &noWriteErrFS{sizes: map[string]int64{"a.bin": 10}}
	fs := Wrap(inner, NewPoolCounter(pool))
	pre, perr := pool.TryReserve(10) // 预置 a.bin 的已占用（须 Commit 才计入 Usage）
	if perr != nil {
		t.Fatal(perr)
	}
	pre.Commit(10)
	// 覆盖写声明 50（净增预留 40），写失败且未落盘 → 占用应保持 10。
	if err := fs.WriteFile(context.Background(), "a.bin", nil, 50, 0); err == nil {
		t.Fatal("应返回写错误")
	}
	if got := pool.Usage(); got != 10 {
		t.Fatalf("未落盘的写失败应回滚预留（占用保持 10），got %d", got)
	}
}

// TestCapacityFS_WriteFailureStatUnverifiableBiasesHigh：Stat 无法复核时**不清账**（宁可
// 偏高——限额宁紧勿松，运维可对账），绝不按「未落盘」静默少计。
func TestCapacityFS_WriteFailureStatUnverifiableBiasesHigh(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	fs := Wrap(&statErrFS{commitThenErrFS{sizes: map[string]int64{}}}, NewPoolCounter(pool))
	if err := fs.WriteFile(context.Background(), "a.bin", nil, 30, 0); err == nil {
		t.Fatal("应返回写错误")
	}
	if got := pool.Usage(); got != 30 {
		t.Fatalf("Stat 不可复核时应保留预留（宁可偏高），got %d", got)
	}
}

// TestCapacityFS_WriteIfAbsentNotWrittenReleases：WriteIfAbsent 显式返回「未写」（幂等命中
// 已存在对象）→ 这是**明确的未落盘答复**，回滚预留是正确口径（与写错误区分）。
func TestCapacityFS_WriteIfAbsentNotWrittenReleases(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	inner := &wiaMeasuringFS{sizes: map[string]int64{"a.bin": 5}}
	fs := Wrap(inner, NewPoolCounter(pool))
	pre, perr := pool.TryReserve(5) // 预置 a.bin 的已占用
	if perr != nil {
		t.Fatal(perr)
	}
	pre.Commit(5)
	ok, err := fs.WriteIfAbsent(context.Background(), "a.bin", strings.NewReader("hello"), 20, 0)
	if err != nil || ok {
		t.Fatalf("幂等命中应 (false, nil), got ok=%v err=%v", ok, err)
	}
	if got := pool.Usage(); got != 5 {
		t.Fatalf("未写应回滚预留（占用保持 5），got %d", got)
	}
}

// TestCapacityFS_DeclaredSizeNilReader：仅声明长度、无内容源（r=nil）的写不得 panic
// （countingReader 空流处理）；按声明长度记账。
func TestCapacityFS_DeclaredSizeNilReader(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	fs := Wrap(&measuringFS{sizes: map[string]int64{}}, NewPoolCounter(pool))
	if err := fs.WriteFile(context.Background(), "a.bin", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile(nil reader): %v", err)
	}
	if got := pool.Usage(); got != 30 {
		t.Fatalf("按声明长度记账，got %d", got)
	}
}

// TestLoad_CorruptSnapshotCounted 回归（评审 F5）：快照损坏必须在**进程级计数**上可见
// （供 /metrics 消费）——此前只有一行 Warn，运维无从区分「限额生效」与「限额已被放宽一轮」
// （反复损坏=反复放大，且启动日志早被淹没）。
func TestLoad_CorruptSnapshotCounted(t *testing.T) {
	t.Parallel()
	before := CorruptSnapshotCount()
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.capacity")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad, 100); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.capacity")
	if err := os.WriteFile(old, []byte(`{"version":99,"used":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(old, 100); err != nil {
		t.Fatal(err)
	}
	if got := CorruptSnapshotCount(); got < before+2 {
		t.Fatalf("损坏+版本不识应各计一次（before=%d got=%d）", before, got)
	}
}
