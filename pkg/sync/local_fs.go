// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/sync/internal/fsutil"
)

// LocalFS 基于 os 实现 FS（Windows 兼容）。
// Root 为本地基准目录；所有 relPath 均为正斜杠相对路径。
//
// 安全边界（审查 MEDIUM：symlink 逃逸）：所有文件操作经 confine() 解析真实路径，
// 要求解析结果（含中间父目录符号链接）仍落在 Root 的规范路径内，否则拒绝。
// 文本 `..` 过滤只是第一道防线；符号链接逃逸由 EvalSymlinks + 前缀校验阻断。
type LocalFS struct {
	Root   string
	Logger *slog.Logger

	rootOnce sync.Once
	rootReal string // Root 的规范路径（EvalSymlinks 解析，失败回落 Abs）
}

// NewLocalFS 创建 LocalFS。
func NewLocalFS(root string, logger *slog.Logger) *LocalFS {
	return &LocalFS{Root: root, Logger: logger}
}

// rootRealPath 返回 Root 的规范路径（解析符号链接），进程内缓存一次。
// Root 不存在时回落 Abs(Root)（WriteFile/MakeDir 会先创建）；仍失败回退原值。
func (l *LocalFS) rootRealPath() string {
	l.rootOnce.Do(func() {
		r, err := filepath.EvalSymlinks(l.Root)
		if err != nil {
			// Root 不存在（如备份目标 user 桶首次写前）：先 MkdirAll 再重试
			// EvalSymlinks——否则 Abs 兜底可能保留 Windows 8.3 短名
			// （RUNNER~1），与后续 EvalSymlinks 展开的长名文本比较不等 →
			// confine 误判「符号链接越界」（#664 Windows CI 实证）。
			if os.MkdirAll(l.Root, 0o755) == nil {
				r, err = filepath.EvalSymlinks(l.Root)
			}
		}
		if err != nil {
			if a, aerr := filepath.Abs(l.Root); aerr == nil {
				r = a
			} else {
				r = l.Root
			}
		}
		l.rootReal = r
	})
	return l.rootReal
}

// within 报告 p 是否等于 root 或位于 root 目录内（前缀 + 分隔符，避免 /a/b 与 /a/bb 误判）。
func within(root, p string) bool {
	if p == root {
		return true
	}
	prefix := root + string(os.PathSeparator)
	if runtime.GOOS == "windows" {
		// Windows 大小写不敏感（RUNNER~1 8.3 短名展开为小写长名，见 #664）
		// + EvalSymlinks 可能返回不同大小写——用 EqualFold 前缀比较避免误判越界。
		return strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix))
	}
	return strings.HasPrefix(p, prefix)
}

// confine 把已 sanitize 的相对路径（clean）映射为 Root 内安全绝对路径，拒绝符号链接逃逸。
//
// 防线（对齐 pkg/storage os.Root 的相对打开语义，审查 MEDIUM 闭环）：
//  1. 拼接 Root 规范路径 + clean；
//  2. EvalSymlinks 解析目标：目标存在（含中间父目录符号链接）且解析结果落在 Root 内
//     → 返回解析后的真实路径；解析结果在 Root 外 → 拒绝；
//  3. 目标不存在（如写入/重命名前）→ 逐级向上解析已存在父目录，任一级符号链接指向
//     Root 外 → 拒绝；
//  4. 文本 `..` 已由 sanitizeRelPath 拦截（纵深防御）。
func (l *LocalFS) confine(clean string) (string, error) {
	rootReal := l.rootRealPath()
	full := filepath.Join(rootReal, filepath.FromSlash(clean))
	if !within(rootReal, full) {
		return "", fmt.Errorf("路径越界根目录: %s", clean)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		// 目标不存在：逐级向上解析已存在的父目录，校验符号链接不越界。
		dir := full
		for dir != rootReal && dir != filepath.Dir(dir) {
			dir = filepath.Dir(dir)
			if r, e := filepath.EvalSymlinks(dir); e == nil {
				if !within(rootReal, r) {
					return "", fmt.Errorf("父目录符号链接指向根目录外: %s", clean)
				}
				rel, _ := filepath.Rel(dir, full)
				return filepath.Join(r, rel), nil
			}
		}
		return full, nil
	}
	if !within(rootReal, resolved) {
		return "", fmt.Errorf("路径符号链接指向根目录外: %s", clean)
	}
	return resolved, nil
}

// ListDir 列出单层目录条目。文件条目计算 SHA-256 checksum，目录条目 checksum 为空。
//
// 性能说明（审查 M10）：这里对每个文件全量读盘算 SHA-256（diff 阶段一次、传输阶段
// 再一次 = 2x 读）。对本地目录正确性优先；远程 HTTPTransport 的 ListDir 应利用服务端
// ChecksumStore 直接返回已存 checksum（/api/files 已带），避免逐文件拉全量。
func (l *LocalFS) ListDir(ctx context.Context, relPath string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return nil, cerr
	}
	dirEntries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(dirEntries))
	for _, de := range dirEntries {
		childRel := joinSlash(clean, de.Name())
		childFull := filepath.Join(full, de.Name())
		info, err := os.Lstat(childFull)
		if err != nil {
			return nil, err
		}
		e := Entry{
			Name:      de.Name(),
			Path:      childRel,
			Size:      info.Size(),
			MTime:     info.ModTime().UnixNano(),
			IsDir:     info.IsDir(),
			IsSymlink: info.Mode()&os.ModeSymlink != 0,
		}
		if !e.IsDir && !e.IsSymlink {
			cs, err := l.computeChecksum(ctx, childRel)
			if err != nil {
				return nil, err
			}
			e.Checksum = cs
			e.ChecksumType = "sha256"
			e.Checksums = map[string]string{"sha256": cs}
		}
		out = append(out, e)
	}
	return out, nil
}

// Stat 返回条目信息。路径不存在时返回 (nil, nil)。
// 符号链接由 os.Stat 跟随（返回目标信息）。
func (l *LocalFS) Stat(ctx context.Context, relPath string) (*Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return nil, cerr
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	e := &Entry{
		Name:  path.Base(clean),
		Path:  clean,
		Size:  info.Size(),
		MTime: info.ModTime().UnixNano(),
		IsDir: info.IsDir(),
	}
	if !info.IsDir() {
		cs, err := l.computeChecksum(ctx, clean)
		if err != nil {
			return nil, err
		}
		e.Checksum = cs
	}
	return e, nil
}

// computeChecksum 流式计算文件 SHA-256（受 ctx 取消约束）。
func (l *LocalFS) computeChecksum(ctx context.Context, rel string) (string, error) {
	full, err := l.confine(rel)
	if err != nil {
		return "", err
	}
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := fsutil.CopyWithCtx(ctx, h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// OpenRead 打开文件读取流（跟随符号链接，但须落在 Root 内）。
func (l *LocalFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return nil, cerr
	}
	return os.Open(full)
}

// OpenRangeRead 实现 RangeReader：定点读取文件的 [offset, offset+size) 区间。
// 用 os.File.ReadAt + 精确限流读取，不加载整文件。区间越出文件 → 错误（fail-closed）。
func (l *LocalFS) OpenRangeRead(ctx context.Context, relPath string, offset, size int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || size < 0 {
		return nil, fmt.Errorf("sync: 非法区间 offset=%d size=%d", offset, size)
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return nil, cerr
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	// 探测文件长并校验区间越界（ReadAt 到 EOF 前少读即越界，此处预检 fail-closed）。
	st, serr := f.Stat()
	if serr != nil {
		f.Close()
		return nil, fmt.Errorf("sync: stat %s 失败: %w", relPath, serr)
	}
	if offset+size > st.Size() {
		f.Close()
		return nil, fmt.Errorf("sync: 区间 [%d,%d) 越出文件大小 %d", offset, offset+size, st.Size())
	}
	if size == 0 {
		f.Close()
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	return &rangeReadCloser{f: f, off: offset, size: size}, nil
}

// rangeReadCloser 是定点读的 ReadCloser：从 offset 起精确读 size 字节。
type rangeReadCloser struct {
	f    *os.File
	off  int64 // 当前读偏移（文件内绝对）
	size int64 // 剩余待读字节
}

func (r *rangeReadCloser) Read(p []byte) (int, error) {
	if r.size == 0 {
		return 0, io.EOF
	}
	// 只读剩余部分（ReadAt 到 EOF 前不足即越界，需精确限流）。
	toRead := min(int64(len(p)), r.size)
	n, err := r.f.ReadAt(p[:toRead], r.off)
	r.off += int64(n)
	r.size -= int64(n)
	if int64(n) < toRead {
		// 实际读少（文件提前 EOF）→ 区间越出文件。
		return n, fmt.Errorf("sync: 范围读取越出文件末尾")
	}
	return n, err // err 为 nil（读满限流量）；到目标末尾后下次 Read 返回 EOF。
}

func (r *rangeReadCloser) Close() error { return r.f.Close() }

// WriteFile 写入文件，自动创建父目录并保留 mtime。大文件拷贝受 ctx 取消约束
// （审查 I-3：本地 FS 阻塞 IO 也尊重取消语义，为远程实现立规范）。
// 原子写（可信卷自愈）：同一目录 CreateTemp → CopyWithCtx → fsync → Rename 覆盖——
// 崩溃/失败只留临时文件或旧完整文件，绝不留下半写目标（转存首写失败后下次覆盖可自愈）。
func (l *LocalFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return err
	}
	// 先建父目录再 confine：目录存在后 confine 走「目标存在」分支（EvalSymlinks
	// 整路径 + within 检查），避免逐级解析不存在中间目录时 Windows 8.3 短名
	// （如 CI 的 RUNNER~1）与长名文本比较不等误判「父目录符号链接指向根目录外」
	// （#664 备份测试 Windows CI 实证）。符号链接越界仍由存在分支 within 拦截。
	preDir := filepath.Dir(filepath.Join(l.rootRealPath(), filepath.FromSlash(clean)))
	if preDir != l.rootRealPath() {
		if mkErr := os.MkdirAll(preDir, 0o755); mkErr != nil {
			return mkErr
		}
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return cerr
	}
	if dir := filepath.Dir(full); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return mkErr
		}
	}
	// 原子写：同目录临时文件 → 流式拷贝 → fsync → rename 覆盖。
	// Windows 同卷 rename 原子；临时文件随原文件名前缀防跨目录 EXDEV；fsync 保证
	// 落盘后才 rename 可见（防断电丢块）。
	return l.writeFileAtomic(ctx, full, r, mtime)
}

// writeFileAtomic 同目录 tmp + fsync + rename 的原子写核心（WriteFile 唯一写路径）。
// 失败清理临时文件，绝不留下半写目标（可信卷自愈语义）。
func (l *LocalFS) writeFileAtomic(ctx context.Context, full string, r io.Reader, mtime int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpPath) }
	if _, err := fsutil.CopyWithCtx(ctx, tmp, r); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if mtime != 0 {
		t := time.Unix(0, mtime)
		if err := os.Chtimes(tmpPath, t, t); err != nil {
			os.Remove(tmpPath)
			return err
		}
	}
	if err := os.Rename(tmpPath, full); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// WriteIfAbsent 原子拒绝已存在写入（W1/W3 转存唯一语义）：O_EXCL 创建——目标已存在
// 返回 (false, nil)；写失败清理临时文件。并发安全（O_EXCL 原子存在性判定）。
func (l *LocalFS) WriteIfAbsent(ctx context.Context, relPath string, r io.Reader, size, mtime int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return false, err
	}
	if dir := filepath.Dir(clean); dir != "." && dir != "" {
		if mkErr := l.MakeDir(ctx, dir); mkErr != nil {
			return false, mkErr
		}
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return false, cerr
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := fsutil.CopyWithCtx(ctx, f, r); err != nil {
		f.Close()
		os.Remove(full)
		return false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(full)
		return false, err
	}
	if mtime != 0 {
		t := time.Unix(0, mtime)
		if cerr := os.Chtimes(full, t, t); cerr != nil {
			return false, cerr
		}
	}
	return true, nil
}

// Rename 重命名/移动文件/目录（from/to 均须落在 Root 内，防符号链接逃逸）。
func (l *LocalFS) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromClean, err := fsutil.SanitizeRelPath(from)
	if err != nil {
		return err
	}
	toClean, err := fsutil.SanitizeRelPath(to)
	if err != nil {
		return err
	}
	fromFull, ferr := l.confine(fromClean)
	if ferr != nil {
		return ferr
	}
	toFull, terr := l.confine(toClean)
	if terr != nil {
		return terr
	}
	return os.Rename(fromFull, toFull)
}

// Move 同卷原子移动（sync.Mover 能力）：等价 os.Rename（源移除、目标覆盖）。
// 跨物理卷/文件系统返回 EXDEV 类错误由调用方回退复制。
func (l *LocalFS) Move(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromClean, err := fsutil.SanitizeRelPath(from)
	if err != nil {
		return err
	}
	toClean, err := fsutil.SanitizeRelPath(to)
	if err != nil {
		return err
	}
	fromFull, ferr := l.confine(fromClean)
	if ferr != nil {
		return ferr
	}
	toFull, terr := l.confine(toClean)
	if terr != nil {
		return terr
	}
	return os.Rename(fromFull, toFull)
}

// Copy 同卷复制（sync.Copier 能力）：源保留、目标新写（读源 → 原子写目标）。
// 实现复用 WriteFile 的 tmp+rename 原子写；调用方保证目标目录存在（WriteFile 自建）。
func (l *LocalFS) Copy(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc, err := l.OpenRead(ctx, from)
	if err != nil {
		return err
	}
	defer rc.Close()
	srcE, err := l.Stat(ctx, from)
	if err != nil || srcE == nil {
		return fmt.Errorf("copy: stat 源 %s: %v", from, err)
	}
	return l.WriteFile(ctx, to, rc, srcE.Size, srcE.MTime)
}

// Link 同卷硬链接（sync.Linker 能力）：目标指向源同一 inode，两名字共享内容。
// 跨物理卷返回 EXDEV 类错误由调用方回退复制（Windows NTFS 文件硬链接可用）。
func (l *LocalFS) Link(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromClean, err := fsutil.SanitizeRelPath(from)
	if err != nil {
		return err
	}
	toClean, err := fsutil.SanitizeRelPath(to)
	if err != nil {
		return err
	}
	fromFull, ferr := l.confine(fromClean)
	if ferr != nil {
		return ferr
	}
	toFull, terr := l.confine(toClean)
	if terr != nil {
		return terr
	}
	if dir := filepath.Dir(toFull); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return mkErr
		}
	}
	return os.Link(fromFull, toFull)
}

// Delete 删除文件（须落在 Root 内，防符号链接逃逸）。
func (l *LocalFS) Delete(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return cerr
	}
	return os.Remove(full)
}

// MakeDir 创建目录（含父目录；父目录链符号链接越界被 confine 拒绝）。
func (l *LocalFS) MakeDir(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return cerr
	}
	return os.MkdirAll(full, 0o755)
}

// OpenReaderAt 实现 BlockAccessor：打开路径随机读（*os.File 是 io.ReaderAt）。
func (l *LocalFS) OpenReaderAt(ctx context.Context, relPath string) (io.ReaderAt, io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return nil, nil, err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return nil, nil, cerr
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, nil, err
	}
	return f, f, nil
}

// OpenWriterAt 实现 BlockAccessor：打开路径随机写（差异块按 offset 写入）。
// 预分配 size（Truncate）；mtime != 0 时 Close 后保留（与 WriteFile 同语义）。
func (l *LocalFS) OpenWriterAt(ctx context.Context, relPath string, size, mtime int64) (io.WriterAt, io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	clean, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return nil, nil, err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return nil, nil, cerr
	}
	if dir := filepath.Dir(full); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, nil, mkErr
		}
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	if size > 0 {
		if terr := f.Truncate(size); terr != nil {
			_ = f.Close()
			return nil, nil, terr
		}
	}
	if mtime != 0 {
		// 预先记录 mtime 值；Close 后由 wrapper 应用（无法在写前设——Truncate 会改）。
		_ = mtime
	}
	return f, &blockWriterCloser{f: f, full: full, mtime: mtime}, nil
}

// blockWriterCloser 包装 *os.File：Close 时先落盘再设 mtime（与 WriteFile 语义对齐）。
type blockWriterCloser struct {
	f     *os.File
	full  string
	mtime int64
}

func (b *blockWriterCloser) WriteAt(p []byte, off int64) (int, error) { return b.f.WriteAt(p, off) }

func (b *blockWriterCloser) Close() error {
	if err := b.f.Close(); err != nil {
		return err
	}
	if b.mtime != 0 {
		t := time.Unix(0, b.mtime)
		return os.Chtimes(b.full, t, t)
	}
	return nil
}

// IsLocalVolume 内部卷自述（syncpkg.LocalVolume 能力接口，用户裁定 2026-10-05）：
// **内部/本地卷必须实现接口说明自己是内部**（返回 true → 转存走用户配额 + 本地化
// 便利）；未实现接口的 FS 默认视为外部卷（远程，容量/配额由卷自身管理）。
func (l *LocalFS) IsLocalVolume() bool { return true }
