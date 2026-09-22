// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package webdavcore 提供仅依赖 pkg/sync 的 WebDAV 适配（NewHandler + webdavFS）。
// 与 pkg/gateway/webdav（含 remote:// 客户端适配）分离：pkg/server 无法 import
// 含 pkg/remote 的包（client e2e_test import server 构成测试期环）。
package webdavcore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/cocomhub/sproxy/pkg/sync"
)

// NewHandler 返回桥接 fs 的 WebDAV http.Handler。
// fs 为 nil 时返回 nil（调用方负责非空校验）。
func NewHandler(fs sync.FS) http.Handler {
	if fs == nil {
		return nil
	}
	return &webdav.Handler{
		FileSystem: &webdavFS{fs: fs},
		LockSystem: webdav.NewMemLS(),
	}
}

// webdavFS 把 sync.FS 桥接为 x/net/webdav 的 FileSystem。
type webdavFS struct {
	fs sync.FS
}

// 编译期断言：webdavFS 实现 webdav.FileSystem。
var _ webdav.FileSystem = (*webdavFS)(nil)

// Mkdir 创建目录（sync.FS.MakeDir）。
// RFC 4918 要求 MKCOL 对已存在资源返回 405：sync.FS.MakeDir 幂等（已存在返回 nil），
// 故此处先 Stat 存在性——已存在返回 os.ErrExist（webdav handler 非 NotExist 错误 → 405）。
func (w *webdavFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	rel := cleanRel(name)
	entry, err := w.fs.Stat(ctx, rel)
	if err != nil {
		return err
	}
	if entry != nil {
		return os.ErrExist
	}
	return w.fs.MakeDir(ctx, rel)
}

// RemoveAll 删除文件/目录树（sync.FS.Delete）。
func (w *webdavFS) RemoveAll(ctx context.Context, name string) error {
	return w.fs.Delete(ctx, cleanRel(name))
}

// Rename 重命名/移动（sync.FS.Rename）。
func (w *webdavFS) Rename(ctx context.Context, oldName, newName string) error {
	return w.fs.Rename(ctx, cleanRel(oldName), cleanRel(newName))
}

// Stat 返回文件元信息；不存在时返回 os.ErrNotExist（webdav handler 依赖它判 404）。
func (w *webdavFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	entry, err := w.fs.Stat(ctx, cleanRel(name))
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, os.ErrNotExist
	}
	return &webdavFileInfo{entry: *entry}, nil
}

// OpenFile 按 flag 分派读写。
//   - 读（无 O_CREATE/O_TRUNC/O_WRONLY）：文件走 fs.OpenRead 全量读入内存（支持 Seek），
//     目录返回目录条目快照（PROPFIND 列举用 Readdir）。
//   - 写（含 O_CREATE/O_TRUNC/O_WRONLY 之一）：返回内存缓冲 writeFile，
//     io.Copy 写入后由 Stat()/Close() 经 fs.WriteFile 落盘（sync.FS 语义为全量覆盖）。
func (w *webdavFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	rel := cleanRel(name)
	isWrite := flag&(os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_RDWR) != 0

	entry, err := w.fs.Stat(ctx, rel)
	if err != nil {
		return nil, err
	}

	if !isWrite {
		// 读路径：目录（PROPFIND 列举）或文件（GET）。
		if entry == nil {
			return nil, os.ErrNotExist
		}
		if entry.IsDir {
			return newDirFile(ctx, w.fs, rel, *entry), nil
		}
		rc, err := w.fs.OpenRead(ctx, rel)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		// 流式读取长度以实际读入为准（sync.FS.Entry.Size 仅作元信息，不用于裁剪——
		// 远端传输可能截断/多传，实际读到的才是 GET 应返回的内容）。
		return &readFile{Reader: bytes.NewReader(data), info: &webdavFileInfo{entry: *entry}}, nil
	}

	// 写路径：内存缓冲，Stat()/Close() 时落盘。
	return &writeFile{ctx: ctx, fs: w.fs, rel: rel, info: entry}, nil
}

// cleanRel 归一 WebDAV 路径为 sync.FS 相对路径（去掉前导 /，拒绝空与 .. 逃逸）。
// 返回 "" 表示根。
func cleanRel(name string) string {
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	trimmed := strings.TrimPrefix(clean, "/")
	if trimmed == "." || trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "../") || trimmed == ".." {
		return "" // 越界路径按根处理（webdav handler 已做 stripPrefix 防穿越，此处兜底）
	}
	return trimmed
}

// webdavFileInfo 实现 os.FileInfo（webdav handler 用 Name/Size/IsDir/ModTime）。
type webdavFileInfo struct {
	entry sync.Entry
}

func (f *webdavFileInfo) Name() string       { return f.entry.Name }
func (f *webdavFileInfo) Size() int64        { return f.entry.Size }
func (f *webdavFileInfo) Mode() os.FileMode  { return modeOf(f.entry) }
func (f *webdavFileInfo) ModTime() time.Time { return time.Unix(0, f.entry.MTime) }
func (f *webdavFileInfo) IsDir() bool        { return f.entry.IsDir }
func (f *webdavFileInfo) Sys() any           { return nil }

// modeOf 由 Entry 派生 os.FileMode（目录加 ModeDir 位）。
func modeOf(e sync.Entry) os.FileMode {
	if e.IsDir {
		return 0o755 | os.ModeDir
	}
	return 0o644
}

// readFile 全量读入内存的 http.File（支持 Seek，GET/Range 用）。
type readFile struct {
	*bytes.Reader
	info os.FileInfo
}

func (f *readFile) Stat() (os.FileInfo, error) { return f.info, nil }
func (f *readFile) Write(_ []byte) (int, error) {
	return 0, fmt.Errorf("webdav readFile: 只读")
}
func (f *readFile) Readdir(_ int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid // 文件不是目录
}
func (f *readFile) Close() error { return nil }

// dirFile 表示目录条目的读打开（PROPFIND 列举）。
type dirFile struct {
	ctx   context.Context
	fs    sync.FS
	rel   string
	info  os.FileInfo
	ents  []os.FileInfo // 一次性快照
	index int
}

func newDirFile(ctx context.Context, fs sync.FS, rel string, e sync.Entry) *dirFile {
	return &dirFile{ctx: ctx, fs: fs, rel: rel, info: &webdavFileInfo{entry: e}}
}

func (d *dirFile) Stat() (os.FileInfo, error) { return d.info, nil }
func (d *dirFile) Write(_ []byte) (int, error) {
	return 0, os.ErrInvalid // 目录不可写
}
func (d *dirFile) Read(_ []byte) (int, error) { return 0, os.ErrInvalid } // 目录不可读
func (d *dirFile) Seek(_ int64, _ int) (int64, error) {
	return 0, os.ErrInvalid
}
func (d *dirFile) Close() error { return nil }

// Readdir 返回目录条目（webdav walkFS 用；count<=0 时全部返回）。
func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if d.ents == nil {
		entries, err := d.fs.ListDir(d.ctx, d.rel)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			infos = append(infos, &webdavFileInfo{entry: e})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
		d.ents = infos
	}
	if count <= 0 {
		out := d.ents[d.index:]
		d.index = len(d.ents)
		return out, nil
	}
	if d.index >= len(d.ents) {
		return nil, io.EOF
	}
	end := min(d.index+count, len(d.ents))
	out := d.ents[d.index:end]
	d.index = end
	return out, nil
}

// writeFile 是写路径的内存缓冲文件：io.Copy 写入缓冲，Stat()/Close() 时经 fs.WriteFile 落盘。
type writeFile struct {
	flushed bool
	ctx     context.Context
	fs      sync.FS
	rel     string
	info    *sync.Entry // 已存在条目（nil = 新建）
	buf     bytes.Buffer
}

func (w *writeFile) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *writeFile) Read(_ []byte) (int, error)  { return 0, os.ErrInvalid }
func (w *writeFile) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekStart {
		return 0, nil
	}
	return 0, fmt.Errorf("webdav writeFile: seek 仅支持起点")
}
func (w *writeFile) Readdir(_ int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }

// Stat 返回写后元信息；尚未落盘时先 flush（webdav handler 在 io.Copy 后先 Stat 再 Close）。
func (w *writeFile) Stat() (os.FileInfo, error) {
	if err := w.flush(); err != nil {
		return nil, err
	}
	entry, err := w.fs.Stat(w.ctx, w.rel)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, os.ErrNotExist
	}
	return &webdavFileInfo{entry: *entry}, nil
}

// Close 落盘缓冲内容（幂等：已 flush 过则不再重复写）。
func (w *writeFile) Close() error {
	return w.flush()
}

// flush 把缓冲内容经 fs.WriteFile 落盘（sync.FS.WriteFile 为全量覆盖，与 PUT 幂等一致）。
func (w *writeFile) flush() error {
	if w.flushed {
		return nil
	}
	w.flushed = true
	var mtime int64
	if w.info != nil {
		mtime = w.info.MTime
	}
	return w.fs.WriteFile(w.ctx, w.rel, bytes.NewReader(w.buf.Bytes()), int64(w.buf.Len()), mtime)
}

// 编译期断言。
var _ webdav.File = (*readFile)(nil)
var _ webdav.File = (*dirFile)(nil)
var _ webdav.File = (*writeFile)(nil)
