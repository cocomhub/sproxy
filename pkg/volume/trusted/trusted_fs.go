// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package trusted 提供可信卷（Trusted Volume）装饰器：包装任意 syncpkg.FS，
// 提供文件配套 meta（FileMeta）读写/校验/隐藏/配额能力——任何卷经 Wrap 包一层
// 即成为可信卷（上传/下载按总哈希 + 分块哈希逐分片校验）。**统一封装、不逐卷改
// 方法**：底层只提供现有 FS 接口，新卷接入 = 包一层，零改卷本身。
//
// 价值定位：可信卷解决「跨信任边界的静默损坏」（外部卷服务端不承诺完整性、
// 上传成功但内容损坏、读回校验和不可信）——FileMeta 是应用层独立校验证据。
// 自建 LocalFS（Stat 已算 sha256）价值低，但装饰器成本也低（meta 落盘可选）。
package trusted

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// Options 是可信卷装饰器配置。
type Options struct {
	// ChunkSize 分块校验大小（<=0 = 按文件大小自适应 ChunkSizeForSize）。
	ChunkSize int64
	// Extra 是写入 meta 的自定义扩展信息（创建人/email 等任意数据）。
	Extra map[string]any
	// Logger 是 meta 落盘/校验失败的可观测日志器（C-MAJOR-7；nil = 静默——默认装配
	// 不传 Logger 保持现状，运维可注入看 meta 丢失/校验异常）。
	Logger *slog.Logger
}

// TrustedVolumeFS 包装任意 syncpkg.FS，提供 meta sidecar + 校验能力。
type TrustedVolumeFS struct {
	inner syncpkg.FS
	opts  Options
}

// transparentUnwrapper 是**透明装饰器**（如 capacity.CapacityFS）暴露内层 FS 的接口。
// trusted.Wrap 通过它下探，判断「**卷自身**是否自带 meta」——否则转发层（实现了
// FileMeta 但只是委托 inner）会被误判为自带 Provider，跳过 TrustedVolumeFS（写出
// 的 sidecar 就没人建了）。
type transparentUnwrapper interface {
	Inner() syncpkg.FS
}

// innermost 逐层剥去透明装饰器，返回最内层 FS。
func innermost(fs syncpkg.FS) syncpkg.FS {
	for {
		u, ok := fs.(transparentUnwrapper)
		if !ok {
			return fs
		}
		nxt := u.Inner()
		if nxt == nil || nxt == fs {
			return fs
		}
		fs = nxt
	}
}

// Wrap 包装 fs 为可信卷。
//
// **用户裁定 2026-10-07：卷自身提供正确 meta 接口的无需封装，交给卷处理**——判断依据是
// **最内层原始卷**是否实现 meta.Provider（下探透明装饰器，避免把 capacity.CapacityFS
// 之类的转发层误当自带 Provider）：是则保持装饰链（外层转发 Provider）；否则包装饰器。
func Wrap(fs syncpkg.FS, opts Options) syncpkg.FS {
	if _, isProvider := fs.(meta.Provider); isProvider {
		if _, transparent := fs.(transparentUnwrapper); !transparent {
			return fs // 原始卷自带 meta（如 secretdata）：不封装，交给卷处理
		}
	}
	if _, innerProvider := innermost(fs).(meta.Provider); innerProvider {
		return fs // 透明装饰器链最内层自带 meta：保持装饰链，不叠加 TrustedVolumeFS
	}
	return &TrustedVolumeFS{inner: fs, opts: opts}
}

// Inner 返回底层 FS（装配层需要原始能力如 Move/Copy/Link 时取用）。
// **预留逃生口**：当前无调用（能力已由装饰器透传）；保留供受控装配直接触达 inner。
func (t *TrustedVolumeFS) Inner() syncpkg.FS { return t.inner }

// FileMeta 实现 meta.Provider（读侧消费 FileMeta 的读端入口——C4 落地）：装饰器写侧
// 已算并落盘 meta sidecar，读侧经它取回 FileMeta 供转存校验/可信卷读校验逐分块比对。
// sidecar 缺失（未启用/旁路写/落盘失败兜底）→ 返回错误，调用方回落 Stat 直算/流式。
// A-MAJOR-4 修复：sidecar 读上限 maxMetaSidecarBytes（防被注入超大 JSON 的 OOM DoS——
// meta 是可信卷信任根，超限按"meta 缺失"回落直算而非崩溃）。
func (t *TrustedVolumeFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	rc, err := t.inner.OpenRead(ctx, meta.MetaPath(rel))
	if err != nil {
		return nil, fmt.Errorf("trusted: 读 meta sidecar %s: %w", meta.MetaPath(rel), err)
	}
	defer rc.Close()
	raw, rerr := io.ReadAll(io.LimitReader(rc, meta.MaxMetaSidecarBytes()+1))
	if rerr != nil {
		return nil, fmt.Errorf("trusted: 读 meta sidecar %s: %w", meta.MetaPath(rel), rerr)
	}
	if int64(len(raw)) > meta.MaxMetaSidecarBytes() {
		return nil, fmt.Errorf("trusted: meta sidecar %s 超限（>%d B，疑似注入），回落直算",
			meta.MetaPath(rel), meta.MaxMetaSidecarBytes())
	}
	fm, uerr := meta.Unmarshal(raw)
	if uerr != nil {
		return nil, fmt.Errorf("trusted: 解析 meta %s: %w", meta.MetaPath(rel), uerr)
	}
	return fm, nil
}

// maxMetaSidecarBytes 已去重：统一用 meta.MaxMetaSidecarBytes()（P3 修复——
// 防两处 64MiB 字面量漂移）。

// UpdateMetaExtra 更新已落盘 meta sidecar 的 Extra（旁路记录能力——如源完整性
// damaged 标记 source_integrity=damaged；失败返回错误由调用方 Warn 兜底，不阻断）。
// 供转存层在任务携带旁路语义时（IntegrityStatus=damaged）把标记写入目标卷 meta，
// 消费者读 FileMeta.Extra 可见。sidecar 缺失 → 错误（调用方跳过）。
// A-MAJOR-3 修复：读-改-写非原子在并发覆盖窗口会拿旧哈希 meta 覆盖新 meta——更新后
// 重读校验 TotalSHA256 仍与磁盘文件一致（检测到并发覆盖则返回错误，调用方跳过而非
// 污染新 meta）。
func (t *TrustedVolumeFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	fm, err := t.FileMeta(ctx, rel)
	if err != nil {
		return err
	}
	// C5 修复（E-MAJOR-2）：**先读回校验、通过后再落盘**——原实现先写旧哈希 meta
	// 再读回比对（A-MAJOR-3），顺序反了：并发覆盖时旧 meta 已覆盖新 meta、读回发现
	// 不一致却**不回滚**，正确新 meta 已丢失（readbackVerify 此后恒失配）。校验前置：
	// 主文件当前内容必须仍与旧 meta 一致（fm.TotalSHA256），一致才允许写入（合并
	// Extra 的 meta 仍描述当前内容，天然无污染）。
	// P1-10：整文件读回交叉校验仅本地卷执行——远端卷（网盘等）回读 = 一次全量下载，
	// 对 6GiB 级文件不可接受；远端并发覆盖窗口由写路径独占 + 读路径校验兜底。
	if t.isLocal() {
		rc, oerr := t.inner.OpenRead(ctx, rel)
		if oerr != nil {
			return nil // 主文件不可读（并发删除等）→ 不误报，调用方跳过
		}
		calc, cerr := meta.NewCalculator(fm.Size, fm.ChunkSize)
		if cerr != nil {
			rc.Close()
			return nil
		}
		_, rerr := calc.ReadFrom(rc)
		rc.Close()
		if rerr != nil {
			return nil
		}
		got := calc.Finish()
		if got.TotalSHA256 != fm.TotalSHA256 {
			// 主文件已被并发覆盖（旧 meta 描述旧内容）→ 返回错误（与函数注释「失败返回
			// 错误由调用方 Warn 兜底」一致，可观测），不覆盖新 meta（无污染）。
			return fmt.Errorf("trusted: 主文件已被并发覆盖（旧 meta 失效），跳过 Extra 更新")
		}
	}
	if fm.Extra == nil {
		fm.Extra = make(map[string]any, len(extra))
	}
	for k, v := range extra {
		fm.Extra[k] = v
	}
	data, merr := meta.Marshal(fm)
	if merr != nil {
		return fmt.Errorf("trusted: meta 序列化失败: %w", merr)
	}
	if werr := t.inner.WriteFile(ctx, meta.MetaPath(rel), bytesReader(data), int64(len(data)), 0); werr != nil {
		return fmt.Errorf("trusted: meta 更新落盘失败: %w", werr)
	}
	// B-MAJOR 修复：写后交叉比对（复用 verifyWriteMeta）——写前校验与落盘之间仍存在
	// 并发覆盖窗口（校验通过 → 合并 Extra → marshal → WriteFile 期间另一写者覆盖主文件
	// + 新 meta），不加这一步会固化"描述旧内容的 meta+extra"盖掉新 meta（读路径恒失配，
	// 与 missing 直算兜底不同）。写后重读主文件比对 TotalSHA256，不一致 → 删 meta 退化
	// missing（下次覆盖写自愈）。
	t.verifyWriteMeta(ctx, rel, fm)
	return nil
}

// ---- 能力接口回显（B-A2 修复）：装饰器必须委托底层能力，否则包装后
// WriteIfAbsent/LocalVolume/ReserveSpace/Mover/Copier/Linker 等断言全落空——转存
// 唯一性/配额/容量语义降级。逐一委托 inner（inner 未实现该接口时返回 false/错误
// 由断言方回落，与直接使用 inner 行为一致）。----

// WriteIfAbsent 委托 inner（转存唯一性原子写；inner 未实现 → 返回 ErrUnsupported，
// 调用方 errors.Is 识别后回落 Stat 检查+WriteFile——与裸 FS 断言失败语义一致）。
// B-MAJOR-2 修复：inner 实现且写成功 → 补写 meta sidecar（「到达即建」不变量——
// 首写转存经此路径也带可信 meta，否则 readbackVerify 的 FileMeta 分块校验不可达）。
func (t *TrustedVolumeFS) WriteIfAbsent(ctx context.Context, path string, r io.Reader, size, mtime int64) (bool, error) {
	pia, ok := t.inner.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("trusted: 底层卷未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	// 流式计算 meta（P1 性能）：对入参 r 包 TeeReader，与 WriteFile 同法——避免写成功后
	// `writeMetaAfter` 整文件回读（远端卷 = 一次全量下载，6GiB → +6GiB，与设计 §8
	// 「远端不回读」直接冲突）。Calculator 无法构造时回落旧回读路径（保可用性）。
	calc, cerr := meta.NewCalculator(size, t.chunkSizeFor(size))
	if cerr != nil {
		written, err := pia.WriteIfAbsent(ctx, path, r, size, mtime)
		if err != nil || !written {
			return written, err
		}
		t.writeMetaAfter(ctx, path, size)
		return written, nil
	}
	written, err := pia.WriteIfAbsent(ctx, path, io.TeeReader(r, calc), size, mtime)
	if err != nil || !written {
		return written, err
	}
	t.persistCalcMeta(ctx, path, calc)
	return written, nil
}

// writeMetaAfter 在写成功路径补建 meta sidecar（WriteIfAbsent 首写专用；WriteFile
// 已内嵌流式计算）。从已落盘文件读回计算（与 filesMetaPolicy.computeMeta 同语义）。
// 失败静默（与 WriteFile meta 落盘失败吞错同语义——meta 缺失读路径直算兜底）。
// meta 恒生成（用户裁定：只关校验不关 meta——sidecar 是完整性证据，写入路径必落）。
func (t *TrustedVolumeFS) writeMetaAfter(ctx context.Context, rel string, size int64) {
	// C2 修复：与 WriteFile 共用 chunkSizeFor（含 minMetaChunkSize 钳制）——原实现只用
	// <=0 默认，配置 ChunkSize=1B + 大文件 → 10 亿 ChunkMeta OOM/爆配额。
	chunkSize := t.chunkSizeFor(size)
	rc, err := t.inner.OpenRead(ctx, rel)
	if err != nil {
		return // 读回失败：meta 不落（直算兜底）
	}
	defer rc.Close()
	calc, cerr := meta.NewCalculator(size, chunkSize)
	if cerr != nil {
		return
	}
	if _, rerr := calc.ReadFrom(rc); rerr != nil {
		return
	}
	t.persistCalcMeta(ctx, rel, calc)
}

// persistCalcMeta 由已完成的 Calculator 落 meta sidecar（WriteFile/WriteIfAbsent 共用）。
// 失败仅影响校验精度（读路径直算兜底），不阻断主写。
func (t *TrustedVolumeFS) persistCalcMeta(ctx context.Context, rel string, calc *meta.Calculator) {
	fm := calc.Finish()
	// size 声明失真的实写自愈（有分块才可能失真）；零字节文件无分块直接 Validate。
	if len(fm.Chunks) > 0 {
		meta.FixSizeFromChunks(fm)
	}
	if meta.Validate(fm) != nil {
		return
	}
	fm.Name = path.Base(filepath.ToSlash(rel))
	// 深拷贝：所有文件的 meta 不得共享同一 map 实例（对比 secretdata 已深拷贝）——
	// 未来任何就地改 Extra 的调用方会污染全部文件的 meta。
	fm.Extra = maps.Clone(t.opts.Extra)
	data, merr := meta.Marshal(fm)
	if merr != nil {
		return
	}
	if werr := t.inner.WriteFile(ctx, meta.MetaPath(rel), bytesReader(data), int64(len(data)), 0); werr != nil {
		t.logMetaWarn("persistCalcMeta", rel, werr)
	}
}

// logMetaWarn 记 meta 落盘/校验失败的可观测日志（C-MAJOR-7：Options.Logger 注入时
// Warn；nil = 静默现状——默认装配不传 Logger 保持零噪音）。meta 失败不阻断主写
// （读路径直算兜底），但运维可经 Logger 看到 meta 丢失/校验异常。
func (t *TrustedVolumeFS) logMetaWarn(stage, rel string, err error) {
	if t.opts.Logger == nil {
		return
	}
	t.opts.Logger.Warn("trusted: meta 落盘失败（读路径直算兜底）", "stage", stage, "path", rel, "error", err)
}

// ReserveSpace 委托 inner（卷容量预检；inner 未实现 → 返回 ErrUnsupported，调用方
// errors.Is 识别后跳过预检——与裸 FS 断言失败一致）。
func (t *TrustedVolumeFS) ReserveSpace(ctx context.Context, relPath string, size int64) error {
	if rs, ok := t.inner.(syncpkg.ReserveSpace); ok {
		return rs.ReserveSpace(ctx, relPath, size)
	}
	return fmt.Errorf("trusted: 底层卷未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
}

// IsLocalVolume 委托 inner（内部/外部卷判定；inner 未实现 → 默认外部，与直接使用一致）。
func (t *TrustedVolumeFS) IsLocalVolume() bool {
	if lv, ok := t.inner.(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}

// WithStagingQuota 委托 inner（StagingQuotaCapable 能力——baidupcs_sync 装配层经通用
// 接口注入 staging 配额，Wrap 装饰后仍可达底层；inner 未实现 → no-op）。
func (t *TrustedVolumeFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if qc, ok := t.inner.(syncpkg.StagingQuotaCapable); ok {
		qc.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 委托 inner（StagingQuotaExempt 能力——s3 流式直传不落本地盘，
// 显式豁免；P0-1 修复：Wrap 装饰后仍透传，否则装配层 stagingQuotaFS 断言落在 Wrap 上
// 恒 false → s3 被 gate 强制预留，大文件 5s 超时误拒）。
func (t *TrustedVolumeFS) ExemptStagingQuota() bool {
	if ex, ok := t.inner.(syncpkg.StagingQuotaExempt); ok {
		return ex.ExemptStagingQuota()
	}
	return false
}

// OpenRangeRead 委托 inner（RangeReader：secretdata 视频关键帧定点读 / baidupcs dlink
// Range GET）。A7 修复：Wrap 装饰后不遮蔽 Range 能力，否则装配层 `Guard(Wrap(fs))`
// 的 Range 读退化为整流。inner 未实现 → ErrUnsupported（与其它能力同口径）。
func (t *TrustedVolumeFS) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	if rr, ok := t.inner.(syncpkg.RangeReader); ok {
		return rr.OpenRangeRead(ctx, p, offset, size)
	}
	return nil, fmt.Errorf("trusted: 底层卷未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
}

// DirectURL 委托 inner（DirectURLProvider：集群出口 302 直链）。A7 修复：Wrap 装饰后
// 不遮蔽直链能力，否则 direct_link 外部卷 302 失效。inner 未实现 → ErrUnsupported。
func (t *TrustedVolumeFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if d, ok := t.inner.(syncpkg.DirectURLProvider); ok {
		return d.DirectURL(ctx, relPath)
	}
	return "", false, fmt.Errorf("trusted: 底层卷未实现 DirectURL: %w", syncpkg.ErrUnsupported)
}

// Move 委托 inner（同卷移动；inner 未实现 → ErrUnsupported，调用方回落复制）。
// 成功后联动移动 sidecar（C1 修复：与 Rename 一致，防目标无 meta、源 meta 成孤儿）。
func (t *TrustedVolumeFS) Move(ctx context.Context, from, to string) error {
	if mv, ok := t.inner.(syncpkg.Mover); ok {
		if err := mv.Move(ctx, from, to); err != nil {
			return err
		}
		// 联动 meta（best-effort：源 meta 不存在则跳过；目标父目录先建——B-MINOR
		// 与 Copy 一致，Move 到新子目录时 meta/<新dir> 可能不存在）。
		t.ensureMetaDir(ctx, meta.MetaPath(to))
		if rerr := t.inner.Rename(ctx, meta.MetaPath(from), meta.MetaPath(to)); rerr != nil {
			// A-MAJOR-6 修复：联动失败记日志（原静默吞错）——源 meta 不存在（未启用/
			// 旁路写）是正常 skip，其余失败可观测（读路径直算兜底，不阻断主 Move）。
			t.logMetaWarn("Move", to, rerr)
			// R1-FS-2：源无 meta 时目标旧 sidecar 描述被覆盖的旧内容 → 失效（防固化不可读）。
			t.invalidateMeta(ctx, to)
		}
		return nil
	}
	return fmt.Errorf("trusted: 底层卷未实现 Move: %w", syncpkg.ErrUnsupported)
}

// Copy 委托 inner（同卷复制；inner 未实现 → ErrUnsupported，调用方回落常规复制）。
// 成功后复制 sidecar（C1 修复：目标文件也带可信 meta）。
func (t *TrustedVolumeFS) Copy(ctx context.Context, from, to string) error {
	if cp, ok := t.inner.(syncpkg.Copier); ok {
		if err := cp.Copy(ctx, from, to); err != nil {
			return err
		}
		// 复制 meta（best-effort：源 meta 不存在则跳过——目标无 meta 由读路径直算兜底）。
		t.copyMeta(ctx, from, to)
		return nil
	}
	return fmt.Errorf("trusted: 底层卷未实现 Copy: %w", syncpkg.ErrUnsupported)
}

// Link 委托 inner（硬链接；inner 未实现 → ErrUnsupported，调用方回落复制）。
// 成功后复制 sidecar（C1 修复：Link 语义源保留——不能 Rename 源 meta，须复制；
// 同 inode 内容必同，目标名共享源 meta）。
func (t *TrustedVolumeFS) Link(ctx context.Context, from, to string) error {
	if lk, ok := t.inner.(syncpkg.Linker); ok {
		if err := lk.Link(ctx, from, to); err != nil {
			return err
		}
		t.copyMeta(ctx, from, to)
		return nil
	}
	return fmt.Errorf("trusted: 底层卷未实现 Link: %w", syncpkg.ErrUnsupported)
}

// copyMeta 复制 sidecar（读源 meta 字节流写目标；失败静默——best-effort，读路径直算兜底）。
func (t *TrustedVolumeFS) copyMeta(ctx context.Context, from, to string) {
	src := meta.MetaPath(from)
	dst := meta.MetaPath(to)
	rc, err := t.inner.OpenRead(ctx, src)
	if err != nil {
		// 源 meta 不存在（读路径直算兜底）；但目标旧 sidecar（若有）现已描述旧内容 → 失效。
		t.invalidateMeta(ctx, to)
		return
	}
	defer rc.Close()
	// 取源 meta 实际大小作为声明长度（避免传 -1 使容量层无法预留/按实测结算；
	// FS-CORE-6）。取不到则仍传 -1（容量层现按实测结算，不再反向记账）。
	srcSize := int64(-1)
	if se, serr := t.inner.Stat(ctx, src); serr == nil && se != nil && !se.IsDir {
		srcSize = se.Size
	}
	// B-MINOR：目标父目录先建（与 Rename 的 MakeDir 一致——Copy 到新子目录时
	// meta/<新dir> 可能不存在，直接 WriteFile 会失败被吞成无 meta 孤儿）。
	t.ensureMetaDir(ctx, dst)
	if werr := t.inner.WriteFile(ctx, dst, rc, srcSize, 0); werr != nil {
		// A-MAJOR-6 修复：联动失败记日志（原静默吞错）——可观测，读路径直算兜底。
		t.logMetaWarn("Copy", to, werr)
		// 目标旧 sidecar 描述旧内容 → 失效（防陈旧 meta 固化）。
		t.invalidateMeta(ctx, to)
	}
}

// invalidateMeta 删除目标 sidecar（覆盖写/移动/复制后源无可用 meta 时，防陈旧 meta
// 描述旧内容使读路径恒失配、文件固化不可读——R1-FS-2）。删除不存在是正常（无旧 sidecar）。
func (t *TrustedVolumeFS) invalidateMeta(ctx context.Context, rel string) {
	if err := t.inner.Delete(ctx, meta.MetaPath(rel)); err != nil {
		t.logMetaWarn("invalidateMeta", rel, err)
	}
}

// ensureMetaDir 确保 sidecar 目标父目录存在（best-effort：失败跳过——WriteFile 失败
// 由调用方读路径直算兜底）。
func (t *TrustedVolumeFS) ensureMetaDir(ctx context.Context, mrel string) {
	if dir := path.Dir(mrel); dir != "." && dir != "" {
		_ = t.inner.MakeDir(ctx, dir)
	}
}

// ListDir 列目录：过滤 meta 桶（sidecar 独立桶用户不可见）；user 桶内真实 `.meta`
// 用户文件合法可见（sidecar 移桶后不再冲突）。
func (t *TrustedVolumeFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	es, err := t.inner.ListDir(ctx, p)
	if err != nil {
		return nil, err
	}
	out := es[:0]
	for _, e := range es {
		if meta.IsMetaPath(e.Path) {
			continue // 隐藏 meta 桶条目（sidecar 用户不可见）
		}
		out = append(out, e)
	}
	return out, nil
}

// Stat 返回条目（过滤 meta 桶；附 Checksums）。
func (t *TrustedVolumeFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if meta.IsMetaPath(p) {
		return nil, nil // meta 桶对用户不可见
	}
	return t.inner.Stat(ctx, p)
}

// OpenRead 打开读取（装饰器本身透传——逐分块校验在消费侧由
// meta.VerifyReadSeeker 完成，不落每个卷）。
func (t *TrustedVolumeFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return t.inner.OpenRead(ctx, p)
}

// WriteFile 写入 + 计算并落 .meta（若启用）。meta 大小计入底层配额（WriteFile 经
// 底层 CapacityFS/账本时自然计入——meta 也经 WriteFile 写）。
// sidecar 统一独立 meta 桶后：用户写 `x.meta` 落 user 桶、sidecar 落 meta 桶不冲突，
// **无需保留 `.meta` 后缀禁用**（用户真实 `.meta` 文件合法，与本地卷一致）。
// chunkSizeFor 计算可信卷分块大小（WriteFile 与 writeMetaAfter 共用——C2 修复防
// 双实现漂移）：委托 meta.ResolveChunkSize（配置 <=0 自适应 / 配置 < 下界钳到下界 /
// 否则用配置值），与本地卷 filesMetaPolicy 同口径。
func (t *TrustedVolumeFS) chunkSizeFor(size int64) int64 {
	return meta.ResolveChunkSize(t.opts.ChunkSize, size)
}

// WriteFile 写入 + 计算并落 .meta（若启用）。meta 大小计入底层配额（WriteFile 经
// 底层 CapacityFS/账本时自然计入——meta 也经 WriteFile 写）。
// sidecar 统一独立 meta 桶后：用户写 `x.meta` 落 user 桶、sidecar 落 meta 桶不冲突，
// **无需保留 `.meta` 后缀禁用**（用户真实 `.meta` 文件合法，与本地卷一致）。
// A-MAJOR-5 修复：chunkSize 下界钳制——配置极小 chunk_size（如 1B）大文件按 1B 分块
// 产生数百万 ChunkMeta（meta JSON GB 级：落盘爆配额/读侧 OOM）。钳到
// minMetaChunkSize（1MiB）保 chunk 数上限；同时按 size 自适应的上限天然（ChunkSizeForSize
// 最大 32MiB）不设额外上限。
func (t *TrustedVolumeFS) WriteFile(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	// 计算 meta：读一遍流（同时喂给 inner 写）。meta 恒生成（用户裁定：只关校验不关
	// meta——桶隔离/凭据保护不随校验开关变化）。
	chunkSize := t.chunkSizeFor(size)
	calc, err := meta.NewCalculator(size, chunkSize)
	if err != nil {
		return err
	}
	// 覆盖写场景的既有内容保护（C6 修复）：底层 inner 多为原子写（tmp+rename/O_TRUNC 全量
	// 覆盖），但部分卷（s3 单 PUT 原子、ftp STOR 覆盖）无原子替换。meta 校验失败时
	// **不得对既有 rel 执行覆盖性 Delete**——否则一次恶意/故障上传永久丢失旧版本+新内容。
	// 方案：校验失败仅返回错误（底层写入已失败/成功由 inner 语义决定），不额外删 rel；
	// 真正需要清理的是「本次写」——内层原子写路径失败本就自愈（不留半写）。
	if err := t.inner.WriteFile(ctx, rel, io.TeeReader(r, calc), size, mtime); err != nil {
		return err
	}
	// 尾块已由 Finish 收尾（Write 路径不足 chunkSize 的尾块 Finish 内 flush）。
	fm := calc.Finish()
	if err := meta.Validate(fm); err != nil {
		// m1 修复：size 声明失真（调用方 Content-Length 撒谎/传输截断）时按实写自愈——
		// 分块哈希是实写内容的真实累计（TotalSHA256/各块 SHA256 必真），唯一可能是 Size
		// 字段（声明值）与分块覆盖和不符 → 重设 Size=分块覆盖和再 Validate，产出基于实写
		// 的正确 meta 并落盘（而非报错留半途文件，客户端重试见已存在 → putCheckExisting
		// 覆盖，重复上传）。仍失败（非 size 类）才返回错误。
		if ok := meta.FixSizeFromChunks(fm); !ok || meta.Validate(fm) != nil {
			// C6：不 Delete(rel)（防删既有覆盖目标）。底层写入已成功但 meta 无法自愈
			// （罕见——非 size 类校验失败），主文件已落盘新内容但无可信 sidecar。
			// C1 修复：**删旧 sidecar**（best-effort）——残留描述旧内容的陈旧 meta 会使
			// 读路径 FileMeta 成功解析旧哈希、按旧分块校验新内容恒失配（硬失败，与
			// missing 直算兜底不同，文件被固化不可读）。删后退化为 missing（直算兜底，
			// 下次覆盖写自愈）。
			_ = t.inner.Delete(ctx, meta.MetaPath(rel))
			return fmt.Errorf("trusted: 写入后 meta 校验失败（size %d vs 实写，sidecar 未落）: %w", size, err)
		}
	}
	fm.Name = path.Base(filepath.ToSlash(rel))
	// 深拷贝：所有文件的 meta 不得共享同一 map 实例（对比 secretdata 已深拷贝）——
	// 未来任何就地改 Extra 的调用方会污染全部文件的 meta。
	fm.Extra = maps.Clone(t.opts.Extra)
	data, merr := meta.Marshal(fm)
	if merr != nil {
		// C1 修复：Marshal 失败同样删旧 sidecar（主文件已写成功）——残留描述旧内容的
		// 陈旧 meta 会使读路径按旧分块校验新内容恒失配（硬失败固化）；删后退化为
		// missing（直算兜底，下次覆盖写自愈）。
		_ = t.inner.Delete(ctx, meta.MetaPath(rel))
		return fmt.Errorf("trusted: meta 序列化失败: %w", merr)
	}
	// E-MAJOR 修复：写 meta 前确保 sidecar 父目录存在（与 Move/Copy/Rename 的
	// ensureMetaDir 一致）——transfer 只 ensureTransferDir(user 目录)，远程卷
	// （库路径 baidupcs 等不自动建父目录）meta/<rel>.meta 父目录未建 → WriteFile 失败
	// → 下方删旧 sidecar → 每次上传 meta 恒缺失，verifyByFileMeta 恒回落流式（可信卷
	// 分块校验在远程卷上静默失效）。
	t.ensureMetaDir(ctx, meta.MetaPath(rel))
	// meta 落盘（隐藏同目录 sidecar）。配额口径（B-MAJOR 如实声明）：
	//   - 本地卷：经 filesMetaPolicy 走 owner meta 桶子 Scope（TryReserve→Commit，占 owner 配额）；
	//   - 外部卷（本装饰器路径）：meta 经 inner.WriteFile 计入**卷自身真实占用**——远程网盘
	//     容量/配额由卷后端自管（超限 WriteFile 失败），不进入 owner 全局 Scope 预留
	//     （外部卷上传 reserveVolume 只按主文件 size 预留；meta 字节由卷容量约束，合理）。
	if werr := t.inner.WriteFile(ctx, meta.MetaPath(rel), bytesReader(data), int64(len(data)), 0); werr != nil {
		// meta 落盘失败：主文件已成功——不失败主写（meta 可下次读时补），记日志语义
		// 由调用方/审计处理；此处返回主写成功（meta 缺失时 Stat 直算兜底）。
		// C1 修复：**删旧 sidecar**（best-effort）——覆盖写场景旧 meta 描述旧内容，
		// 残留会使读路径按旧分块校验新内容恒失配（硬失败固化）；删后退化为 missing。
		// C-MAJOR-7：经 Options.Logger 可观测（注入时 Warn），默认装配 nil 静默。
		_ = t.inner.Delete(ctx, meta.MetaPath(rel))
		t.logMetaWarn("WriteFile", rel, werr)
		return nil
	}
	// M5 修复（E-MAJOR-3）：meta 落盘成功后读回主文件交叉比对 TotalSHA256——主文件与
	// meta 是两次独立原子写，云转存路径不经上层 fileLocks；并发写同 rel 时主文件与 meta
	// 可能来自不同 writer（meta=A、主文件=B → 读路径按 A 分块校验 B 恒失配，永久固化）。
	// 读回重算若与刚落盘的 fm 不一致（被并发覆盖）→ 删 meta（best-effort，退化 missing
	// 直算兜底）而非固化错误 sidecar。
	t.verifyWriteMeta(ctx, rel, fm)
	return nil
}

// verifyWriteMeta 写后读回主文件交叉比对（M5/E-MAJOR-3）：重算 TotalSHA256 与刚落盘的
// fm 比对——不一致（并发覆盖错位）→ 删 meta 退化 missing；一致或读回失败（并发删除等）
// → 保留（读回失败不误删，主文件不可读时 meta 无害）。
func (t *TrustedVolumeFS) verifyWriteMeta(ctx context.Context, rel string, fm *meta.FileMeta) {
	// P1-10：整文件读回型交叉校验仅限本地卷——远端卷每次写都回读 = 一次全量下载
	// （6GiB 级不可行），且写路径已独占 + 读路径逐分块校验兑底。
	if !t.isLocal() {
		return
	}
	rc, oerr := t.inner.OpenRead(ctx, rel)
	if oerr != nil {
		return // 主文件不可读（并发删除等）→ 保留 meta（读路径直算兜底）
	}
	calc, cerr := meta.NewCalculator(fm.Size, fm.ChunkSize)
	if cerr != nil {
		rc.Close()
		return
	}
	_, rerr := calc.ReadFrom(rc)
	rc.Close()
	if rerr != nil {
		return
	}
	got := calc.Finish()
	if got.TotalSHA256 != fm.TotalSHA256 {
		// 主文件被并发覆盖（本次 meta 描述旧写流内容）→ 删 meta 退化 missing（直算兜底，
		// 下次覆盖写自愈），不固化错误 sidecar。
		_ = t.inner.Delete(ctx, meta.MetaPath(rel))
		t.logMetaWarn("WriteFile-verify", rel, fmt.Errorf("meta 与主文件交叉比对不一致（并发覆盖错位），已删 sidecar"))
	}
}

// isLocal 报告底层卷自述为本地/内部卷（未实现 LocalVolume 的按外部卷处理——与全仓
// 「未实现默认外部」一致）。用于把整文件读回型交叉校验（verifyWriteMeta /
// UpdateMetaExtra 前置校验）限制在本地卷，避免远端卷每次写付一次全量下载（P1-10）。
func (t *TrustedVolumeFS) isLocal() bool {
	lv, ok := t.inner.(syncpkg.LocalVolume)
	return ok && lv.IsLocalVolume()
}

// Delete 删除文件 + 联动删除 .meta。
func (t *TrustedVolumeFS) Delete(ctx context.Context, rel string) error {
	err := t.inner.Delete(ctx, rel)
	if err != nil {
		return err // 主文件删除失败（仍在）：不得删 sidecar（否则文件在但永久失证据）
	}
	// 仅主文件确实删除后才联动 meta（best-effort：meta 残留由 GC/Stat 兜底忽略）。
	_ = t.inner.Delete(ctx, meta.MetaPath(rel))
	return nil
}

// Rename 重命名/移动 + 联动 .meta。目标父目录自动创建（与 WriteFile 行为一致——
// 移动文件到未建子目录时避免裸 os.Rename 报"路径不存在"）。
// A-MAJOR-2 修复：meta 目标父目录也确保存在（Move/Copy 的 ensureMetaDir 对 Rename
// 缺位——重命名到全新子目录时 meta sidecar rename 底层失败被静默丢弃，目标无 meta、
// 源 meta 孤儿）。
func (t *TrustedVolumeFS) Rename(ctx context.Context, from, to string) error {
	if dir := path.Dir(to); dir != "." && dir != "" {
		if err := t.MakeDir(ctx, dir); err != nil {
			return err
		}
	}
	if err := t.inner.Rename(ctx, from, to); err != nil {
		return err
	}
	// 联动 meta（best-effort：源 meta 不存在则跳过——rename 目标 meta 若无源也不报错）。
	t.ensureMetaDir(ctx, meta.MetaPath(to))
	if rerr := t.inner.Rename(ctx, meta.MetaPath(from), meta.MetaPath(to)); rerr != nil {
		// A-MAJOR-6 修复：联动失败记日志（原静默吞错）——源 meta 不存在是正常 skip，
		// 其余失败可观测（读路径直算兜底，不阻断主 Rename）。
		t.logMetaWarn("Rename", to, rerr)
		// R1-FS-2：源无 meta / 联动失败时，目标已有旧 sidecar 描述的是被覆盖的旧内容 →
		// 必须失效，否则读路径按旧 meta 校验新内容恒失配（硬失败固化，与 missing 直算不同）。
		t.invalidateMeta(ctx, to)
	}
	return nil
}

// MakeDir 透传（meta 不涉及目录）。
func (t *TrustedVolumeFS) MakeDir(ctx context.Context, p string) error {
	return t.inner.MakeDir(ctx, p)
}

// bytesReader 最小 io.Reader（meta 落盘用）。
type byteReader struct {
	data []byte
	off  int
}

func bytesReader(b []byte) *byteReader { return &byteReader{data: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// 编译期断言：TrustedVolumeFS 实现 sync.FS 与**全部可选能力接口**（能力透传契约，
// 防未来新增能力接口后遗漏——装饰器不转发会让上层断言恒 false → 静默降级）。
// BlockAccessor 有意不实现（与 CapacityFS/Guard 同理由，见设计 §8）。
var (
	_ syncpkg.FS = (*TrustedVolumeFS)(nil)

	_ meta.Provider               = (*TrustedVolumeFS)(nil)
	_ MetaExtraUpdater            = (*TrustedVolumeFS)(nil)
	_ syncpkg.WriteIfAbsent       = (*TrustedVolumeFS)(nil)
	_ syncpkg.ReserveSpace        = (*TrustedVolumeFS)(nil)
	_ syncpkg.LocalVolume         = (*TrustedVolumeFS)(nil)
	_ syncpkg.Mover               = (*TrustedVolumeFS)(nil)
	_ syncpkg.Copier              = (*TrustedVolumeFS)(nil)
	_ syncpkg.Linker              = (*TrustedVolumeFS)(nil)
	_ syncpkg.RangeReader         = (*TrustedVolumeFS)(nil)
	_ syncpkg.DirectURLProvider   = (*TrustedVolumeFS)(nil)
	_ syncpkg.StagingQuotaCapable = (*TrustedVolumeFS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*TrustedVolumeFS)(nil)
)
