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
	"time"
)

// LocalFS 基于 os 实现 FS（Windows 兼容）。
// Root 为本地基准目录；所有 relPath 均为正斜杠相对路径。
type LocalFS struct {
	Root   string
	Logger *slog.Logger
}

// NewLocalFS 创建 LocalFS。
func NewLocalFS(root string, logger *slog.Logger) *LocalFS {
	return &LocalFS{Root: root, Logger: logger}
}

// abs 将安全清理后的相对路径映射为根目录下的绝对路径。
func (l *LocalFS) abs(clean string) string {
	return filepath.Join(l.Root, filepath.FromSlash(clean))
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
	full := l.abs(clean)
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
	full := l.abs(clean)
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
	full := l.abs(rel)
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

// OpenRead 打开文件读取流（跟随符号链接）。
func (l *LocalFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := sanitizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	return os.Open(l.abs(clean))
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
	full := l.abs(clean)
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

// Rename 重命名/移动文件。
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
	return os.Rename(l.abs(fromClean), l.abs(toClean))
}

// Delete 删除文件。
func (l *LocalFS) Delete(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := sanitizeRelPath(relPath)
	if err != nil {
		return err
	}
	return os.Remove(l.abs(clean))
}

// MakeDir 创建目录（含父目录）。
func (l *LocalFS) MakeDir(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := sanitizeRelPath(relPath)
	if err != nil {
		return err
	}
	return os.MkdirAll(l.abs(clean), 0o755)
}
