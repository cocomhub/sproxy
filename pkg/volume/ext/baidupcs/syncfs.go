// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// syncfs.go 是百度网盘 Storage 的 pkg/sync.FS 适配层。
//
// 目标：把 baidupcs.Storage 包装为 pkg/sync 引擎可消费的文件系统视图（7 方法接口），
// 使「本地↔网盘」同步与「本地↔本地」同步共用同一套编排逻辑（WalkEntries/ComputeDiff）。
//
// 中间态约束（用户硬规则）：WriteFile 的输入流先落**本地 staging 临时文件**，再经
// Storage.Put 上传网盘；OpenRead 经 Storage.Get 落到本地临时文件后返回流——网盘侧
// 只存最终文件，所有暂存/断点/缓存都在本地文件系统（temp 目录）。
//
// 方法映射：
//
//	ListDir  → Storage.List（递归全量简化：同步引擎的单层语义由调用方裁剪）
//	Stat     → Storage.Stat（不存在返回 (nil,nil)）
//	OpenRead → Storage.Get（本地临时文件 + 自动清理）
//	WriteFile→ 本地 staging + Storage.Put（mtime 保留）
//	Rename   → Storage.Copy + Storage.Delete（网盘无原子 MOVE 则两步）
//	Delete   → Storage.Delete
//	MakeDir  → 网盘无独立目录纯概念：路径合法即 no-op
package baidupcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// StorageFS 把 Storage 适配为 pkg/sync.FS。
//
// **本地 staging 配额（2026-10-10 用户裁定）**：不在本结构体持有配额句柄（卷是 backend
// 共享单例，per-owner 注入会互相覆盖）。装配层改为**按请求/按 owner 包一层
// `syncpkg.StagingQuotaGateFS`**（per-instance 预留/释放，无共享可变状态）——与卷自身
// 容量配额（`syncpkg.ReserveSpace`/卷 Pool）严格区分：后者管远端网盘容量，前者只记本地
// 暂存字节。
type StorageFS struct {
	s    StorageAPI
	temp string // 本地中间态目录（staging/下载缓存），用户硬约束：只依赖本地 FS
}

// StorageAPI 是 StorageFS 消费的最小接口（P3 只依赖公开方法，与 P2 内部解耦）。
// 由 *Storage 实现（编译期断言见 NewStorageFS）。
type StorageAPI interface {
	Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error)
	Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error)
	Stat(ctx context.Context, key string) (*ObjectMeta, error)
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)
	Delete(ctx context.Context, key string) error
	Copy(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error)
	// Move 服务端移动（sync.Mover 能力；零流量）。服务端不可用返回 ErrUnsupported，
	// 调用方（StorageFS.Rename）回退 Copy+Delete。
	Move(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error)
	// PutFromStagedPath 复用**调用方已暂存**的本地文件路径上传（本地 staging 路径复用：
	// 不再拷第二份；只读 stagedPath，生命周期由调用方负责）。hints 为可选上传预计算
	// （md5/分块列表/size；RandomBlob 标记随机新建内容），nil = 全自算（现状行为）。
	// P1（2026-10-21）：新增 hints 参数——secretdata 交错路径每块密文顺带算 md5，
	// 传入后跳过整块重读（省 O(块) 两遍）+ 跳过 pre-Stat（RandomBlob 秒传命中率 0）。
	PutFromStagedPath(ctx context.Context, key, stagedPath string, size int64, hints *PutHints) (*ObjectMeta, error)
}

// NewStorageFS 构造 StorageFS 适配层。temp 为本地中间态目录（默认 os.TempDir()）。
func NewStorageFS(s StorageAPI, temp string) (*StorageFS, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: storage is nil", ErrInvalidParam)
	}
	if temp == "" {
		temp = os.TempDir()
	}
	if mkErr := os.MkdirAll(temp, 0o755); mkErr != nil {
		return nil, mkErr
	}
	return &StorageFS{s: s, temp: temp}, nil
}

var _ syncpkg.FS = (*StorageFS)(nil)
var _ syncpkg.StagedPathReuse = (*StorageFS)(nil)

// WriteFromStagedPath 实现 syncpkg.StagedPathReuse：把调用方已暂存的本地文件直接上传
// （**零额外落盘**；契约：只读 stagedPath，生命周期由调用方负责）。
func (f *StorageFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	clean, err := f.validatePath(relPath)
	if err != nil {
		return err
	}
	if _, perr := f.s.PutFromStagedPath(ctx, clean, stagedPath, size, nil); perr != nil {
		return mapPCSError(perr)
	}
	_ = mtime // 百度 API 无直接 Chtimes（与 WriteFile 同口径）
	return nil
}

// ListDir 列出目录单层条目（目录+文件混合，不递归）。
// 底层 Storage.List 是库真目录列举（FilesDirectoriesList 单层语义）；
// 同步引擎的 walkDir 自递归完成树遍历，此处不组装子目录。
func (f *StorageFS) ListDir(ctx context.Context, relPath string) ([]syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metas, err := f.s.List(ctx, relPath)
	if err != nil {
		return nil, mapPCSError(err)
	}
	out := make([]syncpkg.Entry, 0, len(metas))
	for _, m := range metas {
		out = append(out, entryFromMeta(m))
	}
	return out, nil
}

// Stat 返回条目信息；不存在返回 (nil, nil)。
func (f *StorageFS) Stat(ctx context.Context, relPath string) (*syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean := strings.TrimPrefix(relPath, "/")
	// 根路径（""）：网盘根恒为目录。
	if clean == "" {
		return &syncpkg.Entry{Name: "", Path: "", IsDir: true}, nil
	}
	m, err := f.s.Stat(ctx, clean)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, mapPCSError(err)
	}
	e := entryFromMeta(*m)
	return &e, nil
}

// OpenRead 打开文件读取流（经 Storage.Get 落本地临时文件，Close 自动清理）。
func (f *StorageFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rc, _, err := f.s.Get(ctx, relPath)
	if err != nil {
		return nil, mapPCSError(err)
	}
	return rc, nil
}

// WriteFile 把流写入网盘：先落本地 staging 临时文件，再经 Storage.Put 上传。
// 中间态只依赖本地 FS（用户硬规则）；mtime 保留到远端元数据。
// quota：装配了 QuotaTracker 时，staging 写入预留 size，Put 成功释放，失败归还。
// validatePath 归一并校验卷内相对路径（逐段拒绝空/`.`/`..`；前导斜杠剥离）。
// 抽为方法：WriteFile 与 WriteFromStagedPath（staging 复用入口）共用同一口径。
func (f *StorageFS) validatePath(relPath string) (string, error) {
	clean := strings.TrimPrefix(relPath, "/")
	if clean == "" {
		return "", fmt.Errorf("%w: invalid path %q", ErrInvalidParam, relPath)
	}
	for seg := range strings.SplitSeq(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: invalid path %q", ErrInvalidParam, relPath)
		}
	}
	return clean, nil
}

func (f *StorageFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, relPath string, r io.Reader, size, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := f.validatePath(relPath)
	if err != nil {
		return err
	}
	// 本地 staging 配额由外层 per-request `syncpkg.StagingQuotaGateFS` 预留/释放
	// （不再由本共享单例记账——避免 per-owner 覆盖）。
	// **staging 路径复用（用户裁定）**：调用方已把本地暂存文件路径放入 ctx
	// （`syncpkg.WithStagedLocalPath`）→ 直接交给 Storage 复用（**零额外落盘**，本地峰值
	// 1×size，与配额 1× 预留口径一致）；否则本函数落一份 staging 后同样以「路径复用」入口
	// 上传（不再由 Storage 内部拷第二份）。
	if staged, ok := syncpkg.StagedLocalPathFrom(ctx); ok {
		if _, perr := f.s.PutFromStagedPath(ctx, clean, staged, size, nil); perr != nil {
			return mapPCSError(perr)
		}
		return nil
	}
	// 1. 落本地 staging（受 ctx 约束的流式拷贝）。
	tmp, err := os.CreateTemp(f.temp, "staging-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, copyErr := io.Copy(tmp, r); copyErr != nil {
		_ = tmp.Close()
		return copyErr
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return closeErr
	}
	// 2. 上传网盘（Storage 内部有界重试 + ETag 复核）。**以 staging 路径复用入口上传**：
	// Storage 不再拷第二份（此前 staging-* + stage-*/basename 两份并存 = 峰值 2×size）。
	// P1（2026-10-21）：ctx 携带 PutHints 时透传（secretdata 交错路径每块密文顺带算
	// md5 + RandomBlob 标记 ⇒ 跳过整块重读与 pre-Stat，省每次上传 2-3 次 API 往返）。
	var hints *PutHints
	if syncpkg.RandomBlobHintsFrom(ctx) {
		// secretdata 单块密文（随机新建内容）：跳过整块重读 + pre-Stat + rapidupload
		// 刷新（命中率理论 0）——但 md5/分块列表仍需自算（上传复核 + 重试必需）。
		// RandomBlob=true 使 stageUploadFromPath 不读回 sliceMD5/分块列表（不需要秒传）。
		hints = &PutHints{RandomBlob: true}
	} else if h, ok := syncpkg.PutHintsFrom(ctx); ok {
		if ph, phok := h.(*PutHints); phok {
			hints = ph
		}
	}
	meta, err := f.s.PutFromStagedPath(ctx, clean, tmpPath, size, hints)
	if err != nil {
		return mapPCSError(err)
	}
	_ = meta
	// 3. mtime 保留：百度 API 无直接 Chtimes，交由 Storage 层后续扩展（当前记录在元数据）。
	_ = mtime
	return nil
}

// Rename 重命名/移动：服务端 Move（源移除、零流量）；服务端不可用回退 Copy+Delete。
func (f *StorageFS) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := f.s.Move(ctx, from, to); err == nil {
		return nil
	} else if !errors.Is(err, ErrUnsupported) {
		return mapPCSError(err)
	}
	if _, err := f.s.Copy(ctx, from, to); err != nil {
		return mapPCSError(err)
	}
	return f.Delete(ctx, from)
}

// Move 同卷服务端移动（sync.Mover 能力）：源移除、零流量（百度 filemanager move）。
func (f *StorageFS) Move(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := f.s.Move(ctx, from, to)
	return mapPCSError(err)
}

// Copy 同卷服务端复制（sync.Copier 能力）：源保留、零流量（百度 filemanager copy）。
func (f *StorageFS) Copy(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := f.s.Copy(ctx, from, to)
	return mapPCSError(err)
}

// Delete 删除文件（幂等：不存在不报错）。
func (f *StorageFS) Delete(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := f.s.Delete(ctx, relPath)
	if err != nil && isNotFound(err) {
		return nil
	}
	return mapPCSError(err)
}

// MakeDir 网盘无独立目录纯概念：路径合法即成功（no-op）。
func (f *StorageFS) MakeDir(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := sanitizeRemotePath(relPath)
	return err
}

// entryFromMeta 把 ObjectMeta 映射为 sync.Entry（Path 相对 FS 根、正斜杠）。
func entryFromMeta(m ObjectMeta) syncpkg.Entry {
	name := path.Base(strings.TrimSuffix(m.Key, "/"))
	if name == "." || name == "/" {
		name = ""
	}
	e := syncpkg.Entry{
		Name:  name,
		Path:  strings.TrimSuffix(m.Key, "/"),
		Size:  m.Size,
		IsDir: m.IsDir,
	}
	if !m.ModTime.IsZero() {
		e.MTime = m.ModTime.UnixNano()
	}
	// 校验和信息（用户裁定：每个 entry 提供已知的所有校验和数据，便于比较）：
	// **百度 ETag 不能无条件标注为整文件 md5**（M3 实测：分片上传后百度 Stat.MD5 是
	// 片 md5 组合/服务端标记"可能不正确"，非整文件 md5；仅小文件单传（<4MB）ETag 恰为
	// 整文件 md5，但 Stat 无法区分来源）。标 "etag"（与 s3 一致，诚实表达服务端对象
	// 标识）——Equal 的 commonChecksumAlgo 只看 sha256/md5，baidupcs 无交集 → 强制
	// 流式分段自算（跨信任边界不信任远端自报 hash，C2 方向）。Put 路径自身已 readback
	// 验证内容（C5），不受影响。
	if m.ETag != "" {
		e.Checksum = m.ETag
		e.ChecksumType = "etag"
		e.Checksums = map[string]string{"etag": m.ETag}
	}
	return e
}

// isNotFound 判断错误是否为「不存在」语义（哨兵或文本）。
// M6 修复：用 stdlib errors.Is（%w 包装的 ErrNotFound 也能沿 Unwrap 链识别）替代
// 本地恒等比较——errorsIs = err == target 对 fmt.Errorf("%w:...") 包装的错误恒判
// false（文案兜底掩盖了大部分场景，但属错误处理坏味道 + 未来隐患）。
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") || strings.Contains(msg, "不存在")
}
