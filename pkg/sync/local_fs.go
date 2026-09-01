// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
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
	return strings.HasPrefix(p, root+string(os.PathSeparator))
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

// sanitizeRelPath 校验并规范化 relPath：
//   - 拒绝空字节、绝对路径（/、\、盘符）、路径穿越（..）、Windows 非法字符
//   - Windows 上把反斜杠归一为正斜杠
//   - 返回正斜杠形式的清洗后相对路径（"" 表示根）
func sanitizeRelPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("路径包含空字节")
	}
	if p == "" {
		return "", nil
	}
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, "\\", "/")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("路径不能是绝对路径: %s", p)
	}
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("路径不能是绝对路径（盘符）: %s", p)
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return "", fmt.Errorf("无效路径: %s", p)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("路径穿越拒绝: %s", p)
	}
	if runtime.GOOS == "windows" {
		const invalidChars = `<>:"|?*`
		for _, c := range cleaned {
			if strings.ContainsRune(invalidChars, c) {
				return "", fmt.Errorf("路径包含非法字符 %q: %s", c, p)
			}
		}
	}
	return cleaned, nil
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
	clean, err := sanitizeRelPath(relPath)
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
	clean, err := sanitizeRelPath(relPath)
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

// copyWithCtx 流式拷贝并周期性检查 ctx.Done()，支持大文件传输/哈希的取消
// （审查 I-3：LocalFS 是阻塞 IO，纯 io.Copy 无法在取消时中断；这里每 64KiB 让出一次）。
// 返回已拷贝字节与错误；ctx 取消时返回 ctx.Err()。
func copyWithCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			total += int64(nw)
			if ew != nil {
				return total, ew
			}
			if nr != nw {
				return total, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return total, nil
			}
			return total, er
		}
	}
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
	if _, err := copyWithCtx(ctx, h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// OpenRead 打开文件读取流（跟随符号链接，但须落在 Root 内）。
func (l *LocalFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := sanitizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return nil, cerr
	}
	return os.Open(full)
}

// WriteFile 写入文件，自动创建父目录并保留 mtime。大文件拷贝受 ctx 取消约束
// （审查 I-3：本地 FS 阻塞 IO 也尊重取消语义，为远程实现立规范）。
func (l *LocalFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := sanitizeRelPath(relPath)
	if err != nil {
		return err
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
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := copyWithCtx(ctx, f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if mtime != 0 {
		t := time.Unix(0, mtime)
		if err := os.Chtimes(full, t, t); err != nil {
			return err
		}
	}
	return nil
}

// Rename 重命名/移动文件/目录（from/to 均须落在 Root 内，防符号链接逃逸）。
func (l *LocalFS) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromClean, err := sanitizeRelPath(from)
	if err != nil {
		return err
	}
	toClean, err := sanitizeRelPath(to)
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

// Delete 删除文件（须落在 Root 内，防符号链接逃逸）。
func (l *LocalFS) Delete(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := sanitizeRelPath(relPath)
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
	clean, err := sanitizeRelPath(relPath)
	if err != nil {
		return err
	}
	full, cerr := l.confine(clean)
	if cerr != nil {
		return cerr
	}
	return os.MkdirAll(full, 0o755)
}
