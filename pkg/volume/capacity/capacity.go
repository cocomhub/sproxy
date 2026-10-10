// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package capacity 提供外部卷**卷级容量记账与强制**（C2 外部卷容量纳管）。
//
// 用户定案（2026-09-18 / 2026-10-10）：外部卷（配置卷 volumes[] 与用户卷
// /api/volumes/user，含 baidupcs/webdav/s3/secretdata）的本系统可用限额用**卷级计数**
// 强制——写入累计（超限 fail-closed 拒绝）、删除/覆盖/改名释放；与本地卷（owner_quotas
// 物理资源）不同，外部卷不占本机磁盘，限额是「本系统授权占用外部卷的额度」。
//
// **记账位于 FS 层**（`CapacityFS` 包在 backend FS 之外）：凡经 `be.FS()` 的写路径
// （HTTP 上传、云转存、同步 push、备份、服务端 Copy/Move）都被同一卷级计数器拦截，
// 从而保证「所有用户在该卷的占用之和 ≤ 卷限额」。装配层配置卷用 `PoolCounter`
// （复用卷容量 `quota.Pool`，与路由/指标同源）；用户卷用持久化 `VolumeCapacityCounter`
// （`<root>/<owner>/meta/volume/<name>.capacity`——**非 .json**，避开 UserVolumeStore
// 的 *.json 扫描；重启恢复）。
//
// 组件：
//   - Counter：卷级计数抽象（TryAdd/Release/Used/Capacity）。
//   - VolumeCapacityCounter：持久化文件态计数器（用户卷）。
//   - PoolCounter：quota.Pool 适配（配置卷，与 vol_capacity 池同源）。
//   - CapacityFS：sync.FS 装饰器（覆盖写差分 + 透传全部可选能力 + 变更后持久化）。
//   - Backend：把 ExternalBackend 的 FS 包为 CapacityFS 的 backend 包装。
package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// ErrVolumeFull 是卷容量超限哨兵（fail-closed：调用方 errors.Is 识别并映射 507）。
var ErrVolumeFull = errors.New("volume capacity exceeded")

// Counter 是卷级容量记账抽象。
//
// TryAdd 在写入前累计 size，超限返回（包装 ErrVolumeFull）且不改变 used；
// Release 在删除/覆盖/改名后释放；Used/Capacity 供查询。
type Counter interface {
	TryAdd(size int64) error
	Release(size int64)
	Used() int64
	Capacity() int64
}

// VolumeCapacityCounter 是外部卷的卷级容量计数器（C2，持久化文件态）。
//
// used = 本系统当前占用该卷的字节（写入累计 - 删除/覆盖释放）；capacity = 本系统可用
// 限额（UserVolume.Capacity；0 = 不限）。TryAdd 超限拒绝（fail-closed，不部分写入）。
// 持久化：Save 原子写（tmp+rename）；Load 恢复（重启不丢）。
type VolumeCapacityCounter struct {
	mu       sync.Mutex
	used     int64
	capacity int64  // 0 = 不限制
	path     string // 持久化路径（空 = 不持久化）
}

// NewCounter 创建计数器（capacity 字节限额；0 = 不限；path 空 = 不持久化）。
func NewCounter(capacity int64, path string) *VolumeCapacityCounter {
	return &VolumeCapacityCounter{capacity: capacity, path: path}
}

// TryAdd 尝试累计 size 字节（写入前调用）。超限（capacity>0 且 used+size>capacity）
// 返回（包装 ErrVolumeFull）错误（fail-closed：调用方不得写入）。失败不改变 used。
func (c *VolumeCapacityCounter) TryAdd(size int64) error {
	if size <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity > 0 && c.used+size > c.capacity {
		return fmt.Errorf("%w (used %d + %d > limit %d)", ErrVolumeFull, c.used, size, c.capacity)
	}
	c.used += size
	return nil
}

// Release 释放 size 字节（删除/覆盖后调用）。非负归零：释放超过 used → 置 0（幂等防御）。
func (c *VolumeCapacityCounter) Release(size int64) {
	if size <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used -= size
	if c.used < 0 {
		c.used = 0
	}
}

// Used 返回当前占用字节。
func (c *VolumeCapacityCounter) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Capacity 返回限额（0 = 不限）。
func (c *VolumeCapacityCounter) Capacity() int64 { return c.capacity }

// counterFile 是持久化快照（版本化）。
type counterFile struct {
	Version  int   `json:"version"`
	Used     int64 `json:"used"`
	Capacity int64 `json:"capacity"`
}

// Save 原子写持久化（tmp + rename）。path 为空 → no-op（内存态）。
//
// state-write-exempt: 快照路径由调用方给定（用户卷为 <root>/<owner>/meta/volume/...），
// 属**卷数据/计数**而非 pkg/state 状态存储（与 pkg/server/user_volume_store.go 同类）。
func (c *VolumeCapacityCounter) Save() error {
	if c.path == "" {
		return nil
	}
	// 全程持锁（含序列化与落盘）：否则并发 Save 各自在锁内取快照、锁外写文件，
	// 更旧的快照可能后落盘 → 磁盘 used 永久偏低（重启后超限保护被削弱；P2 对抗评审）。
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeCounterFile(c.path, counterFile{Version: 1, Used: c.used, Capacity: c.capacity})
}

// writeCounterFile 原子写计数快照（tmp + fsync + rename）；目录自动创建。
func writeCounterFile(path string, f counterFile) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("capacity: 序列化失败: %w", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return fmt.Errorf("capacity: 创建目录失败: %w", mkErr)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "capacity-*.tmp")
	if err != nil {
		return fmt.Errorf("capacity: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("capacity: 写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("capacity: fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("capacity: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("capacity: 原子重命名失败: %w", err)
	}
	return nil
}

// Load 从 path 恢复计数器。文件不存在 → 新计数器（used=0）。capacity 以调用方传入为准
// （配置权威；持久化的 capacity 仅校验一致，不一致告警但以传入为准）。
func Load(path string, capacity int64) (*VolumeCapacityCounter, error) {
	c := NewCounter(capacity, path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("capacity: 读取 %s 失败: %w", path, err)
	}
	var f counterFile
	if err := json.Unmarshal(data, &f); err != nil {
		// 损坏快照：重置（不 fail-closed——容量是防超限非数据完整，重置后重新累计安全）。
		return c, nil
	}
	c.mu.Lock()
	c.used = f.Used
	c.mu.Unlock()
	return c, nil
}

// VolumeCounterPath 返回配置外部卷容量计数器的持久化路径。
// 目录 <root>/meta/vol-capacity/、文件名 <name>.capacity（**非 .json**——避开
// UserVolumeStore 的 *.json 扫描；与用户卷 <name>.capacity 同口径）。
func VolumeCounterPath(storageRoot, name string) string {
	return filepath.Join(storageRoot, "meta", "vol-capacity", name+".capacity")
}

// PoolCounter 是 quota.Pool 的 Counter 适配（配置卷：与 vol_capacity 池同源，
// 使路由排序/指标/对账读取到的 Usage 即卷级真实占用）。
//
// path 非空时具备**持久化**：装配时把快照 used 预置进池（重启后仍从已占用起算，
// 不漏计）、变更后原子写回——修对抗评审 P1：此前纯内存，重启 Usage 归零 → 可
// 反复写满限额（“Σ用户 ≤ 卷限额”静默失效）。
type PoolCounter struct {
	pool *quota.Pool
	path string
	mu   sync.Mutex
}

// NewPoolCounter 包装卷容量池为卷级计数器（无持久化）。
func NewPoolCounter(p *quota.Pool) *PoolCounter { return &PoolCounter{pool: p} }

// NewPoolCounterPersistent 包装卷容量池并恢复持久化占用：快照 used > 0 时预置进池
// （TryReserve + Commit）；超出当前限额（配置改小）时封顶为限额。文件不存在 → used=0。
func NewPoolCounterPersistent(p *quota.Pool, path string) (*PoolCounter, error) {
	c := &PoolCounter{pool: p, path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("capacity: 读取 %s 失败: %w", path, err)
	}
	var f counterFile
	if json.Unmarshal(data, &f) != nil || f.Used <= 0 {
		return c, nil
	}
	seed := f.Used
	if max := p.MaxBytes(); max > 0 && seed > max {
		seed = max
	}
	if res, err := p.TryReserve(seed); err == nil {
		res.Commit(seed)
	}
	return c, nil
}

// TryAdd 预留并立即提交 size（超限返回包装 ErrVolumeFull 的错误）；变更后持久化。
func (p *PoolCounter) TryAdd(size int64) error {
	if size <= 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	res, err := p.pool.TryReserve(size)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVolumeFull, err)
	}
	res.Commit(size)
	p.persistLocked()
	return nil
}

// Release 释放 size（从池 committed 扣减）；变更后持久化。
func (p *PoolCounter) Release(size int64) {
	if size <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pool.ReleaseCommitted(size)
	p.persistLocked()
}

// persistLocked 在持锁下取快照并原子写回（best-effort；无 path → no-op）。
func (p *PoolCounter) persistLocked() {
	if p.path == "" {
		return
	}
	_ = writeCounterFile(p.path, counterFile{Version: 1, Used: p.pool.Usage(), Capacity: p.pool.MaxBytes()})
}

// Used 返回池当前占用。
func (p *PoolCounter) Used() int64 { return p.pool.Usage() }

// Capacity 返回池上限（0 = 不限）。
func (p *PoolCounter) Capacity() int64 { return p.pool.MaxBytes() }

// CapacityFS 是外部卷 sync.FS 装饰器（卷级记账强制 + 能力透传）。
//
// 记账规则（覆盖所有 FS 变更面，防重复计数）：
//   - WriteFile：stat 旧文件大小 prev，净增 delta=size-prev；delta>0 先 TryAdd（超限拒绝），
//     写成功后若 delta<0 释放；写失败回滚 delta。
//   - WriteIfAbsent：目标不存在先 TryAdd(size)，返回 (false,nil)/err 时回滚。
//   - Delete：stat 旧大小，删除成功后释放。
//   - Rename/Move：目标旧大小在成功后释放（源已在账上）。
//   - Copy：先 TryAdd(源大小)，成功后释放目标旧大小；失败回滚。
//   - Link：硬链接共享 inode，不新增字节，不计。
//
// 变更后 best-effort 持久化（counter 实现 Save 时）。
type CapacityFS struct {
	inner   syncpkg.FS
	counter Counter
}

// Wrap 包装 fs 为带卷级计数的 FS（counter 为任意 Counter 实现）。
func Wrap(fs syncpkg.FS, c Counter) *CapacityFS {
	return &CapacityFS{inner: fs, counter: c}
}

// Counter 返回底层计数器（查询/持久化）。
func (f *CapacityFS) Counter() Counter { return f.counter }

// Inner 返回被包装的底层 FS（透明装饰器下探标记——trusted.Wrap 据此区分「转发层」
// 与「卷自带 meta」，避免把 CapacityFS 的 FileMeta 转发误判为自带 Provider）。
func (f *CapacityFS) Inner() syncpkg.FS { return f.inner }

// persist 变更后 best-effort 持久化（仅带 Save 的计数器，如文件态）。
func (f *CapacityFS) persist() {
	if p, ok := f.counter.(interface{ Save() error }); ok {
		_ = p.Save()
	}
}

// innerSize 返回 rel 当前大小（不存在/目录 → 0）。
func (f *CapacityFS) innerSize(ctx context.Context, rel string) int64 {
	if e, err := f.inner.Stat(ctx, rel); err == nil && e != nil && !e.IsDir {
		return e.Size
	}
	return 0
}

func (f *CapacityFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.inner.ListDir(ctx, p)
}

func (f *CapacityFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.inner.Stat(ctx, p)
}

func (f *CapacityFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.inner.OpenRead(ctx, p)
}

func (f *CapacityFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	prev := f.innerSize(ctx, relPath)
	// 声明长度已知（>0）时先预留（超限 fail-closed 拒绝写入）；未知/非法（<=0，如
	// 分块传输 ContentLength=-1 / copyMeta）不预留，写后按**实测字节**结算。
	reserved := int64(0)
	if size > 0 {
		if d := size - prev; d > 0 {
			if err := f.counter.TryAdd(d); err != nil {
				return err
			}
			reserved = d
		}
	}
	cr := &countingReader{r: r}
	if err := f.inner.WriteFile(ctx, relPath, cr, size, mtime); err != nil {
		if reserved > 0 {
			f.counter.Release(reserved) // 写失败回滚预留
		}
		return err
	}
	// 结算：以实际写入（实测）为准，扣掉已预留。effective <= 0 时才回落声明值
	// （inner 忽略 reader 但仍写 size 字节的极端实现）。
	effective := cr.n
	if effective <= 0 && size > 0 {
		effective = size
	}
	if adj := (effective - prev) - reserved; adj > 0 {
		_ = f.counter.TryAdd(adj) // 声明失真（少报）→ 补记（best-effort）
	} else if adj < 0 {
		f.counter.Release(-adj) // 覆盖写变小 / 多预留
	}
	f.persist()
	return nil
}

// countingReader 记录底层实际被读取的字节（记账以实测为准，防声明长度撒谎）。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// WriteIfAbsent 原子唯一写 + 卷级记账（目标不存在先预留 size）。
func (f *CapacityFS) WriteIfAbsent(ctx context.Context, relPath string, r io.Reader, size, mtime int64) (bool, error) {
	pia, ok := f.inner.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("capacity: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	exists := f.innerSize(ctx, relPath) > 0
	if !exists {
		if err := f.counter.TryAdd(size); err != nil {
			return false, err
		}
	}
	written, err := pia.WriteIfAbsent(ctx, relPath, r, size, mtime)
	if err != nil || !written {
		if !exists {
			f.counter.Release(size) // 未落盘：回滚预留
		}
		return written, err
	}
	f.persist()
	return true, nil
}

func (f *CapacityFS) Rename(ctx context.Context, from, to string) error {
	targetOld := f.innerSize(ctx, to) // 覆盖目标旧字节（成功后释放）
	if err := f.inner.Rename(ctx, from, to); err != nil {
		return err
	}
	f.counter.Release(targetOld)
	f.persist()
	return nil
}

func (f *CapacityFS) MakeDir(ctx context.Context, relPath string) error {
	return f.inner.MakeDir(ctx, relPath)
}

func (f *CapacityFS) Delete(ctx context.Context, relPath string) error {
	size := f.innerSize(ctx, relPath)
	if err := f.inner.Delete(ctx, relPath); err != nil {
		return err
	}
	f.counter.Release(size)
	f.persist()
	return nil
}

// ---- 可选能力透传（装饰器透明转发，否则包装后能力静默降级）----

// Move 委托 inner（Mover）；成功后释放目标旧字节。
func (f *CapacityFS) Move(ctx context.Context, from, to string) error {
	mv, ok := f.inner.(syncpkg.Mover)
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
	}
	targetOld := f.innerSize(ctx, to)
	if err := mv.Move(ctx, from, to); err != nil {
		return err
	}
	f.counter.Release(targetOld)
	f.persist()
	return nil
}

// Copy 委托 inner（Copier）；先预留源大小，成功后释放目标旧字节。
func (f *CapacityFS) Copy(ctx context.Context, from, to string) error {
	cp, ok := f.inner.(syncpkg.Copier)
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
	}
	targetOld := f.innerSize(ctx, to)
	added := f.innerSize(ctx, from)
	if added > 0 {
		if err := f.counter.TryAdd(added); err != nil {
			return err
		}
	}
	if err := cp.Copy(ctx, from, to); err != nil {
		if added > 0 {
			f.counter.Release(added)
		}
		return err
	}
	f.counter.Release(targetOld)
	f.persist()
	return nil
}

// Link 委托 inner（Linker）；硬链接共享 inode，不新增字节（不计）。
func (f *CapacityFS) Link(ctx context.Context, from, to string) error {
	lk, ok := f.inner.(syncpkg.Linker)
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
	}
	return lk.Link(ctx, from, to)
}

// ReserveSpace 委托 inner（卷自管理容量预检；inner 未实现 → ErrUnsupported）。
func (f *CapacityFS) ReserveSpace(ctx context.Context, p string, size int64) error {
	if rs, ok := f.inner.(syncpkg.ReserveSpace); ok {
		return rs.ReserveSpace(ctx, p, size)
	}
	return fmt.Errorf("capacity: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
}

// IsLocalVolume 委托 inner（未实现默认外部）。
func (f *CapacityFS) IsLocalVolume() bool {
	if lv, ok := f.inner.(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}

// OpenRangeRead 委托 inner（RangeReader）。
func (f *CapacityFS) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	if rr, ok := f.inner.(syncpkg.RangeReader); ok {
		return rr.OpenRangeRead(ctx, p, offset, size)
	}
	return nil, fmt.Errorf("capacity: 底层未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
}

// DirectURL 委托 inner（DirectURLProvider）。
func (f *CapacityFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if d, ok := f.inner.(syncpkg.DirectURLProvider); ok {
		return d.DirectURL(ctx, relPath)
	}
	return "", false, fmt.Errorf("capacity: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
}

// FileMeta 委托 inner（meta.Provider：卷自带/装饰器 meta）。
func (f *CapacityFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if pv, ok := f.inner.(meta.Provider); ok {
		return pv.FileMeta(ctx, rel)
	}
	return nil, fmt.Errorf("capacity: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
}

// UpdateMetaExtra 委托 inner（damaged/transfer_verified 旁路标记）。
func (f *CapacityFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	ue, ok := f.inner.(interface {
		UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error
	})
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 UpdateMetaExtra: %w", syncpkg.ErrUnsupported)
	}
	return ue.UpdateMetaExtra(ctx, rel, extra)
}

// WithStagingQuota 委托 inner（StagingQuotaCapable 自管；per-instance 语义）。
func (f *CapacityFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if qc, ok := f.inner.(syncpkg.StagingQuotaCapable); ok {
		qc.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 委托 inner（StagingQuotaExempt 显式豁免）。
func (f *CapacityFS) ExemptStagingQuota() bool {
	if ex, ok := f.inner.(syncpkg.StagingQuotaExempt); ok {
		return ex.ExemptStagingQuota()
	}
	return false
}

// Backend 把外部卷 backend 的 FS 包为 CapacityFS（卷级记账），并暴露 Usage/Capacity 供
// 查询（registry.UsageProvider）。**内嵌 registry.ExternalBackend**：backend 级可选能力
// （HealthProbe/VolumeStatsProvider/Presigner/URLResolver）透明转发，不因记账包装而隐藏。
type Backend struct {
	registry.ExternalBackend
	fs *CapacityFS
}

// WrapBackend 包装外部卷 backend（FS 经 CapacityFS 记账）。
func WrapBackend(be registry.ExternalBackend, c Counter) *Backend {
	return &Backend{ExternalBackend: be, fs: Wrap(be.FS(), c)}
}

// FS 返回带卷级记账的 FS。
func (b *Backend) FS() syncpkg.FS { return b.fs }

// Usage 返回本系统已占用该卷的字节（registry.UsageProvider，权威源 = 本层计数器）。
func (b *Backend) Usage() int64 { return b.fs.Counter().Used() }

// Capacity 返回本系统可用限额（0 = 不限）。
func (b *Backend) Capacity() int64 { return b.fs.Counter().Capacity() }

// Counter 返回底层计数器。
func (b *Backend) Counter() Counter { return b.fs.Counter() }

// ---- backend 级可选能力透传（inner 未实现 → syncpkg.ErrUnsupported 哨兵，
// 消费方 errors.Is 识别后按「不支持」处理，避免包装后能力静默降级）----

// Ping 委托 inner（HealthProbe）。
func (b *Backend) Ping(ctx context.Context) error {
	if hp, ok := b.ExternalBackend.(registry.HealthProbe); ok {
		return hp.Ping(ctx)
	}
	return fmt.Errorf("capacity: backend 未实现 Ping: %w", syncpkg.ErrUnsupported)
}

// Stats 委托 inner（VolumeStatsProvider）。
func (b *Backend) Stats(ctx context.Context) (*registry.VolumeStats, error) {
	if sp, ok := b.ExternalBackend.(registry.VolumeStatsProvider); ok {
		return sp.Stats(ctx)
	}
	return nil, fmt.Errorf("capacity: backend 未实现 Stats: %w", syncpkg.ErrUnsupported)
}

// OpenURL 委托 inner（URLResolver）。
func (b *Backend) OpenURL(ctx context.Context, url string) (io.ReadCloser, error) {
	if ur, ok := b.ExternalBackend.(registry.URLResolver); ok {
		return ur.OpenURL(ctx, url)
	}
	return nil, fmt.Errorf("capacity: backend 未实现 OpenURL: %w", syncpkg.ErrUnsupported)
}

// PresignedURL 委托 inner（Presigner）。
func (b *Backend) PresignedURL(ctx context.Context, relPath, method string, expires int64) (string, error) {
	if p, ok := b.ExternalBackend.(registry.Presigner); ok {
		return p.PresignedURL(ctx, relPath, method, expires)
	}
	return "", fmt.Errorf("capacity: backend 未实现 PresignedURL: %w", syncpkg.ErrUnsupported)
}

// SecretsManagerAny 委托 inner（secrets 卷的 Manager 反取）。
// 用 any 而非具体类型，避免本包依赖具体 secrets 实现；装配层 secrets.ManagerOfExternal
// 反取（未实现 → nil）。修对抗评审 P1：
// 此前 WrapBackend 只提升 {FS,Close}，动态值的 SecretsManager 不在提升集 →
// ManagerOfExternal 断言恒失败 → 配置的 `type: secrets` 卷不可达、密钥被写到默认卷。
func (b *Backend) SecretsManagerAny() any {
	if sp, ok := b.ExternalBackend.(interface{ SecretsManagerAny() any }); ok {
		return sp.SecretsManagerAny()
	}
	return nil
}

// 编译期断言：CapacityFS 实现 sync.FS；Backend 的 FS 满足 sync.FS。
var (
	_ syncpkg.FS = (*CapacityFS)(nil)
	_ Counter    = (*VolumeCapacityCounter)(nil)
	_ Counter    = (*PoolCounter)(nil)
)
