// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// root_fs.go 把 `storage.Root` 适配为 `syncpkg.FS`——**唯一**按卷根语义读写的适配器。
//
// 为什么必须走 Root（评审 P0 的结构性修复）：
//   - DAV 等面此前用 `sync.NewLocalFS(<绝对路径>)`（纯 os I/O）⇒ ① 绕过 at-rest 加解密
//     （读下发密文、写落入明文）；② 绕过**封印**（封装卷占用的源目录仍可经 DAV 直连）。
//   - 经 Root 入口后两件事自动成立：Root 的 OpenDecrypted/OpenFileEncrypted 提供用户可见
//     明文语义，Root 的封印检查对读写一致生效（写恒拒 ErrSealed / 读按声明 404）。
//
// 键空间：调用方用 `syncpkg.NewPrefixFS(core, "user/")` 包一层做桶前缀映射（DAV 相对路径
// → user 桶），PrefixFS 同时负责可选能力转发（R21）。
type rootFSCore struct {
	root *storage.Root
}

// IsLocalVolume 实现 syncpkg.LocalVolume：卷根 FS 恒**本地**（R16 P1 修复①）。
// 此前缺此自述 ⇒ 经 trusted.Guard/Wrap 透传后 pkg/cloud 转存判定为「远程」⇒ 绕过
// owner 配额探测 + 卷级容量池 0 记账（漏记 + 删除幻影释放）。本地根恒本地 ⇒ true。
func (c *rootFSCore) IsLocalVolume() bool { return true }

// NewRootFS 构造「卷根 → sync.FS」适配器（键空间为卷根相对路径）。
func New(root *storage.Root) syncpkg.FS { return &rootFSCore{root: root} }

// rootRel 把 `sync.FS` 的**卷根**相对路径（空串）映射为 `storage.Root` 所需的 `"."`。
//
// 为什么必须映射：`sync.FS` 契约里**空串即卷根**（`LocalFS` 的 `SanitizeRelPath("")` 返回 ""、
// `WalkEntries(ctx, fs, "", …)` 与 `backup.Run` 均以 "" 起步）；而本适配器的底座
// `os.Root` 要求非空相对路径——`Open("")`/`Stat("")` 会以 `openat : empty path` 失败 ⇒
// 经本适配器「列/stat 卷根」此前**恒失败**（`sync.WalkEntries` 从 "" 起步的调用方全中）。
// 只映射空串；`"."` 保持不变（`storage.Root` 对 "." 语义正确，且既有调用方（如
// `path.Dir` 结果）会传 "."）。
func rootRel(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// ListDir 列举目录（Path 为完整相对路径；文件 Size 用**用户可见**长度）。
func (c *rootFSCore) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	des, err := c.root.ReadDir(rootRel(p))
	if err != nil {
		return nil, err
	}
	out := make([]syncpkg.Entry, 0, len(des))
	for _, de := range des {
		rel := path.Join(p, de.Name())
		e := syncpkg.Entry{Name: de.Name(), Path: rel, IsDir: de.IsDir()}
		if de.IsDir() {
			if info, ierr := de.Info(); ierr == nil {
				e.MTime = info.ModTime().UnixNano()
			}
			out = append(out, e)
			continue
		}
		if size, serr := c.root.LogicalSize(rel); serr == nil {
			e.Size = size
		}
		if info, ierr := de.Info(); ierr == nil {
			e.MTime = info.ModTime().UnixNano()
		}
		out = append(out, e)
	}
	return out, nil
}

// Stat 返回条目（不存在 → (nil, nil)，符合 sync.FS 契约）。
func (c *rootFSCore) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fi, err := c.root.Stat(rootRel(p))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	e := &syncpkg.Entry{Name: path.Base(p), Path: p, IsDir: fi.IsDir(), MTime: fi.ModTime().UnixNano()}
	if !fi.IsDir() {
		size, serr := c.root.LogicalSize(rootRel(p))
		if serr != nil {
			return nil, serr
		}
		e.Size = size
	}
	return e, nil
}

// OpenRead 打开用于**用户可见读取**（加密卷透明解密）。
func (c *rootFSCore) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return OpenUserVisibleRead(c.root, rootRel(p))
}

// WriteFile 以用户可见语义写入（加密卷透明加密落盘；封印路径恒拒）。
func (c *rootFSCore) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir := path.Dir(p); dir != "." && dir != "" {
		if err := c.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	w, err := WriteUserVisible(c.root, rootRel(p), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, cerr := io.Copy(w, r); cerr != nil {
		_ = w.Close()
		return cerr
	}
	if cerr := w.Close(); cerr != nil {
		return cerr
	}
	if mtime > 0 {
		mt := time.Unix(0, mtime)
		_ = c.root.Chtimes(rootRel(p), mt, mt) // best-effort：时间不影响内容正确性
	}
	return nil
}

// Rename 重命名/移动（封印双向生效：禁止移入/移出封装区）。
func (c *rootFSCore) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir := path.Dir(to); dir != "." && dir != "" {
		if err := c.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return c.root.Rename(rootRel(from), rootRel(to))
}

// Delete 删除文件/空目录（封印路径恒拒）。
func (c *rootFSCore) Delete(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.root.Remove(rootRel(p))
}

// MakeDir 递归创建目录（封印路径恒拒）。
func (c *rootFSCore) MakeDir(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == "" || p == "." {
		return nil
	}
	return c.root.MkdirAll(rootRel(p), 0o755)
}
