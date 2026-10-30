// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// secrets_fs.go 是「**卷内 secrets/ 目录** → secrets 卷视图」的适配器：把 secrets 读写接回
// `storage.Root`（而不是裸 `os` I/O 的 `sync.NewLocalFS`）。
//
// 为什么必须经 Root（门禁 R26 的登记项在此闭环）：secrets 目录落在**受管卷根之下**——默认
// per-owner 桶是 `<storage_root>/<owner>/meta/secrets`（默认卷根之下的租户 meta 桶内），显式
// 声明的 `type: secrets` 卷目标同样在卷根内。裸 `NewLocalFS` 会让该目录绕过 Root 的
// ① at-rest 透明加解密 ② 封印 ③ 用户可见长度/容量接线。后果对**加密默认卷**尤其严重：卷内容
// 全加密，而**密钥文件明文落盘** ⇒ at-rest 加密对该配置形同虚设（拿到磁盘即拿到密钥）。此前
// 该缺陷记在 R26 白名单的「待裁定」里。
//
// **无兼容负担（用户裁定 2026-10-13：尚未上线）**：secrets 一律**加密形态**（经 Root 写），
// 不做「历史明文识别 / 读时迁移」那类兼容分支——非加密容器按存储层既有报错直接失败
// （fail-closed，不静默降级、不自动改写）。若未来上线后需要迁移，再按彼时的迁移口径单独立项。
//
// 该 FS 是**叶子适配器**（不包装其它 FS 链，故无 R21 能力转发义务）；secrets 只需
// OpenRead/WriteFile/ListDir/Stat/Delete 等方法。所有路径都经 Root ⇒ 封印随之生效。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"

	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// rootSecretsFS 把 `<卷根>/<dirRel>` 目录暴露为 secrets 键空间（键 = 裸 secret 名）。
type rootSecretsFS struct {
	root   *storage.Root
	dirRel string // 卷根相对目录（如 "meta/secrets"）
	logger *slog.Logger
}

// NewRootSecretsFS 构造经根适配的 secrets 视图（dirRel 为空 → 卷根自身，用于 `extra.root`
// 直接指向卷根的配置）。
func NewRootSecretsFS(root *storage.Root, dirRel string, logger *slog.Logger) syncpkg.FS {
	if logger == nil {
		logger = slog.Default()
	}
	return &rootSecretsFS{root: root, dirRel: path.Clean(dirRel), logger: logger}
}

// rel 把 secret 名映射为卷根相对路径（dirRel 为空 → 名本身）。
func (f *rootSecretsFS) rel(name string) string {
	if f.dirRel == "" || f.dirRel == "." {
		return name
	}
	return f.dirRel + "/" + name
}

// OpenRead 打开 secret：加密卷透明解密（用户可见语义）+ 封印生效。
// 非加密容器（如陈旧明文文件）由存储层报错直接失败——**无兼容迁移**（见文件头）。
func (f *rootSecretsFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rootfs.OpenUserVisibleRead(f.root, f.rel(p))
}

// WriteFile 经 Root 写入（加密卷透明加密落盘；封印路径恒拒）。
func (f *rootSecretsFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, _, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rel := f.rel(p)
	if dir := path.Dir(rel); dir != "." && dir != "" {
		if err := f.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// 落盘 0644 → 由 `secrets.Manager` 的本地加固回调收紧为 0600（与既有「LocalFS 写 + chmod」
	// 两步语义一致：加固是显式步骤，经 Root 后由 `ChmodSecret` 回调承接，见 secrets.WithLocalChmod）。
	w, err := rootfs.WriteUserVisible(f.root, rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, cerr := io.Copy(w, r); cerr != nil {
		_ = w.Close()
		return cerr
	}
	return w.Close()
}

// ListDir 列举 secret 名（封印过滤生效；Size 用用户可见长度）。
func (f *rootSecretsFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rel := f.rel(p)
	des, err := f.root.ReadDir(rel)
	if err != nil {
		return nil, err
	}
	out := make([]syncpkg.Entry, 0, len(des))
	for _, de := range des {
		child := path.Join(rel, de.Name())
		e := syncpkg.Entry{Name: de.Name(), Path: path.Join(p, de.Name()), IsDir: de.IsDir()}
		if info, ierr := de.Info(); ierr == nil {
			e.MTime = info.ModTime().UnixNano()
		}
		if !de.IsDir() {
			if size, serr := f.root.LogicalSize(child); serr == nil {
				e.Size = size
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// Stat 返回单个 secret 的条目（不存在 → (nil, nil)，与 sync.FS 契约一致）。
func (f *rootSecretsFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rel := f.rel(p)
	fi, err := f.root.Stat(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	e := &syncpkg.Entry{Name: path.Base(p), Path: p, IsDir: fi.IsDir(), MTime: fi.ModTime().UnixNano()}
	if !fi.IsDir() {
		if size, serr := f.root.LogicalSize(rel); serr == nil {
			e.Size = size
		}
	}
	return e, nil
}

// Delete 删除 secret（封印路径恒拒）。
func (f *rootSecretsFS) Delete(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.root.Remove(f.rel(p))
}

// MakeDir 建目录（secrets 键空间为扁平名字，仅需支持父目录创建语义）。
func (f *rootSecretsFS) MakeDir(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == "" || p == "." {
		return nil
	}
	return f.root.MkdirAll(f.rel(p), 0o755)
}

// Rename 重命名（secrets 场景不使用；实现以保持 sync.FS 契约完整，封印双向生效）。
func (f *rootSecretsFS) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.root.Rename(f.rel(from), f.rel(to))
}

// chmodSecretPath 把 secret 文件权限收紧为 0600（ssh 密钥式本地加固）。与
// `pkg/volume/secrets` 的本地 chmod 同口径（同一 mode；Windows 上 os.Chmod 仅处理只读位，
// 不报错）。
func chmodSecretPath(abs string) error {
	return os.Chmod(abs, 0o600)
}

// ChmodSecret 收紧单个 secret 文件权限（0600，ssh 密钥式）。secrets.Manager 的本地权限语义
// 经该回调接入（FS 接口没有 chmod 能力，且为它扩能力需要全链转发——用装配层回调更小更清晰）。
// 非本地/无权限语义由调用方决定是否传入。
func (f *rootSecretsFS) ChmodSecret(name string) error {
	abs, ok := f.root.Abs(f.rel(name))
	if !ok {
		return nil
	}
	return chmodSecretPath(abs)
}

// 编译期断言：secrets 视图必须是完整 sync.FS（secrets.Manager 只用到其中五个方法，但契约要全）。
var _ syncpkg.FS = (*rootSecretsFS)(nil)
