// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// LayoutVersion 是当前存储布局版本。OpenRoot 时校验/写入 <root>/LAYOUT_VERSION。
const LayoutVersion = "2"

// layoutVersionFile 是布局版本标记文件名。
const layoutVersionFile = "LAYOUT_VERSION"

// Root 封装 os.Root，附带布局版本校验。所有文件操作相对 root，防穿越/符号链接逃逸由标准库保证。
// os.Root 已覆盖 MkdirAll/Chtimes（Go 1.26），本封装直接委托；Abs 派生绝对路径供 os.SameFile 等。
type Root struct {
	r    *os.Root
	base string // 绝对路径（供 Abs/Chtimes 等派生）
	// enc 是 at-rest 加密态（roadmap P2 残余）：非 nil 时 Open/OpenFile 返回
	// 解密 reader / 加密 writer（透明）。OpenRoot 后经 SetEncryption 注入。
	enc *rootEnc
}

// rootEnc 是加密态（key 32B + GCM 复用 + 分块大小——cipher 选型）。
type rootEnc struct {
	key   []byte
	chunk int
}

// OpenRoot 打开 storage root 目录并校验/写入 LAYOUT_VERSION。
// 版本文件不存在则写入当前 LayoutVersion；存在但内容不匹配则返回错误（迁移钩子）。
// path 必须已存在（由装配层创建），未创建时 os.OpenRoot 报错。
func OpenRoot(path string) (*Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("storage: OpenRoot(%s): %w", path, err)
	}
	base, err := filepath.Abs(path)
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("storage: Abs(%s): %w", path, err)
	}
	rt := &Root{r: r, base: base}
	if err := rt.ensureLayoutVersion(); err != nil {
		r.Close()
		return nil, err
	}
	return rt, nil
}

// ensureLayoutVersion 读取/写入布局版本标记：不存在则写入，不匹配则报错。
// 走 os.Root.ReadFile/WriteFile 保持 root 相对且符号链接不逃逸。
func (rt *Root) ensureLayoutVersion() error {
	data, err := rt.r.ReadFile(layoutVersionFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rt.r.WriteFile(layoutVersionFile, []byte(LayoutVersion+"\n"), 0o644)
		}
		return fmt.Errorf("storage: 读取 LAYOUT_VERSION: %w", err)
	}
	if got := strings.TrimSpace(string(data)); got != LayoutVersion {
		return fmt.Errorf("storage: LAYOUT_VERSION 不匹配: 当前 %q 期望 %q", got, LayoutVersion)
	}
	return nil
}

// Open 相对 root 打开文件（原始 *os.File 签名，供 Seek/Stat/Sync 等具体方法）。
func (rt *Root) Open(rel string) (*os.File, error) {
	return rt.r.Open(rel)
}

// OpenFile 相对 root 打开/创建文件（原始 *os.File 签名）。
func (rt *Root) OpenFile(rel string, flag int, perm os.FileMode) (*os.File, error) {
	return rt.r.OpenFile(rel, flag, perm)
}

// OpenDecrypted 打开并解密读取（at-rest 加密卷透明读；非加密态 = Open 等价）。
func (rt *Root) OpenDecrypted(rel string) (io.ReadCloser, error) {
	f, err := rt.r.Open(rel)
	if err != nil {
		return nil, err
	}
	if rt.enc == nil {
		return f, nil
	}
	dr, derr := newEncRootReader(rt.enc.key, f)
	if derr != nil {
		f.Close()
		return nil, derr
	}
	fi, ferr := f.Stat()
	if ferr != nil {
		f.Close()
		return nil, ferr
	}
	return &encReadCloser{r: dr, c: f, size: fi.Size(), open: func() (io.Reader, error) {
		nf, oerr := rt.r.Open(rel)
		if oerr != nil {
			return nil, oerr
		}
		ndr, nderr := newEncRootReader(rt.enc.key, nf)
		if nderr != nil {
			nf.Close()
			return nil, nderr
		}
		return &encReadCloser{r: ndr, c: nf, size: fi.Size()}, nil
	}}, nil
}

// OpenFileEncrypted 打开并加密写（at-rest 加密卷透明写；非加密态 = OpenFile 等价）。
func (rt *Root) OpenFileEncrypted(rel string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	f, err := rt.r.OpenFile(rel, flag, perm)
	if err != nil {
		return nil, err
	}
	if rt.enc == nil {
		return f, nil
	}
	ew, werr := newEncRootWriter(rt.enc.key, f, rt.enc.chunk)
	if werr != nil {
		f.Close()
		return nil, werr
	}
	return &encWriteCloser{w: ew, c: f}, nil
}

// SetEncryption 注入 at-rest 加密态（key 必须 32B；nil 清除 = 明文）。
func (rt *Root) SetEncryption(key []byte) error {
	if key == nil {
		rt.enc = nil
		return nil
	}
	if len(key) != 32 {
		return fmt.Errorf("storage: 加密卷 key 必须 32B")
	}
	rt.enc = &rootEnc{key: key, chunk: 64 << 10}
	return nil
}

// SetCipher 设置加密卷分块大小（roadmap P2 加密归档插件化残余：
// volumes[].extra.cipher 选型——未来 RegisterCipher 扩展算法时块大小取自注册表；
// 当前仅 aes-256-gcm 64KiB 一种，非法块大小 fail-closed）。
// 必须在 SetEncryption 之后调用；未加密卷（enc==nil）时忽略（零回归）。
func (rt *Root) SetCipher(chunk int) error {
	if rt.enc == nil {
		return nil
	}
	if chunk <= 0 {
		return fmt.Errorf("storage: 加密卷块大小非法 %d", chunk)
	}
	rt.enc.chunk = chunk
	return nil
}

// Stat 返回相对路径的文件信息。
func (rt *Root) Stat(rel string) (os.FileInfo, error) {
	return rt.r.Stat(rel)
}

// Lstat 返回相对路径的文件信息（不跟随符号链接）。供 rmdir 等检查符号链接，
// os.Root 保证结果不逃逸 root。
func (rt *Root) Lstat(rel string) (os.FileInfo, error) {
	return rt.r.Lstat(rel)
}

// ReadDir 相对 root 读取目录条目（按名排序）。os.Root 对每路径分量强制 O_NOFOLLOW，
// 中间目录符号链接指向 root 外时返回错误（不逃逸）。
func (rt *Root) ReadDir(rel string) ([]os.DirEntry, error) {
	f, err := rt.r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// MkdirAll 相对 root 递归创建目录；已存在时幂等。
func (rt *Root) MkdirAll(rel string, perm os.FileMode) error {
	return rt.r.MkdirAll(rel, perm)
}

// Remove 相对 root 删除单个文件/空目录。
func (rt *Root) Remove(rel string) error {
	return rt.r.Remove(rel)
}

// RemoveAll 相对 root 递归删除。
func (rt *Root) RemoveAll(rel string) error {
	return rt.r.RemoveAll(rel)
}

// Link 相对 root 创建硬链接（newname 指向 oldname 的同一 inode）。
// 委托 os.Root.Link：两路径均相对 root，符号链接目标不逃逸由标准库保证。
// 硬链接仅同卷内可用（跨物理卷/文件系统会返回 EXDEV）。
func (rt *Root) Link(oldRel, newRel string) error {
	return rt.r.Link(oldRel, newRel)
}

// Rename 相对 root 重命名/移动。
func (rt *Root) Rename(oldRel, newRel string) error {
	return rt.r.Rename(oldRel, newRel)
}

// Chtimes 相对 root 修改访问/修改时间。委托 os.Root.Chtimes，随标准库保证 root 内约束。
func (rt *Root) Chtimes(rel string, atime, mtime time.Time) error {
	return rt.r.Chtimes(rel, atime, mtime)
}

// Abs 派生 root 内 rel 对应的绝对路径并确认仍在 base 内（字符串级校验，供 os.SameFile 等）。
// rel 必须相对（拒绝绝对路径与 .. 段，反斜杠视为分隔符）；非法返回 ("", false)。
func (rt *Root) Abs(rel string) (string, bool) {
	if rel == "" {
		return rt.base, true
	}
	norm := strings.ReplaceAll(rel, `\`, "/")
	if strings.HasPrefix(norm, "/") || filepath.IsAbs(norm) {
		return "", false
	}
	if v := filepath.VolumeName(norm); v != "" {
		return "", false
	}
	clean := filepath.Clean(norm)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	abs := filepath.Join(rt.base, clean)
	if abs != rt.base && !strings.HasPrefix(abs, rt.base+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

// AbsPath 返回 root 基目录的绝对路径（物理路径；供装配层传参给需要落盘路径的
// 组件，如限流协调计数文件目录）。base 在 OpenRoot 时固定为 path 的绝对形式。
func (rt *Root) AbsPath() string { return rt.base }

// Close 关闭 root 句柄。
func (rt *Root) Close() error {
	return rt.r.Close()
}

// encReadCloser 解密流 + 底层文件关闭。
// 实现 io.ReadSeeker 的最小集：Seek 仅支持 0 起（重开解密流）——加密流不可任意 seek
// （块随机存取需重读密文块），Range 语义降级为整读重开（ServeContent 兼容）。
type encReadCloser struct {
	r    io.Reader
	c    io.Closer
	open func() (io.Reader, error) // 重开回调（Seek 0 时重建解密流）
	size int64                     // 文件总大小（密文 size，opaque；Seek End 返回）
	pos  int64                     // 已读位置（Seek End 语义返回）
}

func (e *encReadCloser) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.pos += int64(n)
	return n, err
}
func (e *encReadCloser) Close() error { return e.c.Close() }

func (e *encReadCloser) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart && offset == 0 {
		if e.open == nil {
			return 0, fmt.Errorf("storage: 解密流不支持重开")
		}
		_ = e.c.Close()
		r, err := e.open()
		if err != nil {
			return 0, err
		}
		e.r = r
		e.pos = 0
		return 0, nil
	}
	if whence == io.SeekEnd && offset == 0 {
		return e.size, nil
	}
	return 0, fmt.Errorf("storage: 解密流仅支持 Seek(0, Start) 与 Seek(0, End)")
}

// encWriteCloser 加密流 + 底层文件关闭。
type encWriteCloser struct {
	w io.WriteCloser
	c io.Closer
}

func (e *encWriteCloser) Write(p []byte) (int, error) { return e.w.Write(p) }
func (e *encWriteCloser) Close() error {
	werr := e.w.Close()
	cerr := e.c.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
