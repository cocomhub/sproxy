// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package federated 实现联邦卷只读后端（roadmap 3.3 P2 联邦卷 F1 片）。
//
// 联邦卷 = 把远端 hub 的卷以只读挂载暴露到本地卷视图。本包是**只读适配层**
// （不触碰装配/网络）：Reader 接口由装配层注入（经 FileClient 隧道调远端 hub
// API），federated.FS 把它适配为 sync.FS（写方法恒 ErrReadOnly——fail-closed）。
package federated

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ErrReadOnly 是联邦卷只读语义的哨兵错误（写方法恒返回；调用方按只读处理）。
var ErrReadOnly = errors.New("federated: 联邦卷只读（写操作不支持）")

// ErrVersionedUnsupported 是写面不支持版本感知写入却配置 version/conflict 模式的哨兵错误
// （fail-closed：禁静默降级回 lww——安全开关可观测红线）。
var ErrVersionedUnsupported = errors.New("federated: 写面不支持版本感知写入（VersionedWriter）")

// ConflictMode 是联邦卷写冲突策略（对应配置 extra.conflict_mode）。
type ConflictMode string

const (
	// ConflictLWW 是默认策略（零回归）：写面直转，远端以最后写入者胜。
	ConflictLWW ConflictMode = "lww"
	// ConflictVersion 是版本检查策略：写前 CheckVersion，写时经 WriteFileVersioned
	// CAS（远端当前版本 == expected 才写并 +1，否则 VersionConflictError 409）。
	ConflictVersion ConflictMode = "version"
	// ConflictConflict 是冲突文件策略：写前查版本，远端已存在 → 旧文件改名
	// `<path>.conflict-<ts>` 保留双方，新内容写原路径。
	ConflictConflict ConflictMode = "conflict"
)

// ParseConflictMode 解析 extra.conflict_mode 配置："" / lww → ConflictLWW（零回归）；
// version / conflict 大小写不敏感；非法值报错（fail-closed——禁静默回落 lww 掩盖拼写错误）。
func ParseConflictMode(s string) (ConflictMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "lww":
		return ConflictLWW, nil
	case "version":
		return ConflictVersion, nil
	case "conflict":
		return ConflictConflict, nil
	default:
		return "", fmt.Errorf("federated: 非法 conflict_mode %q（支持 lww/version/conflict）", s)
	}
}

// VersionedWriter 是版本感知写面（可选接口，装配层 type-assert）：version/conflict 模式
// 要求注入的 Writer 实现它。CAS 语义（版本比较 + 写后版本 +1）由实现方（远端写面）保证，
// 本层只编排「先查后写」——TOCTOU 由远端原子比较消除。
type VersionedWriter interface {
	syncpkg.FS
	// CheckVersion 返回远端路径当前版本号（-1 = 不存在）；错误 = 无法判定（fail-closed）。
	CheckVersion(ctx context.Context, path string) (int64, error)
	// WriteFileVersioned 带版本前置检查写入（expected<0 = 期望不存在）；
	// 远端当前版本 != expected → VersionConflictError（409 + 当前版本号提示重取）。
	WriteFileVersioned(ctx context.Context, path string, r io.Reader, size, mtime, expected int64) error
}

// VersionConflictError 是 version 模式 CAS 失败的哨兵错误：远端当前版本号 != 期望版本
// （并发写入者先到）。Current 携带当前版本号供客户端重取重试（幂等）。
// 版本号溢出/异常由实现方视作 CAS 失败（同样返回本错误）——fail-closed 409。
type VersionConflictError struct {
	Path    string
	Current int64
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("federated: %s 版本冲突（当前版本 %d 已变更，请重取后重试）", e.Path, e.Current)
}

// Reader 是联邦卷读能力的注入接口（装配层实现：经 FileClient 隧道调远端 hub）。
type Reader interface {
	// ListDir 列远端卷内目录（path 为卷内相对路径，"" = 卷根）。
	ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error)
	// Stat 取远端卷内路径元信息。
	Stat(ctx context.Context, path string) (*syncpkg.Entry, error)
	// OpenRead 打开远端卷内路径读流。
	OpenRead(ctx context.Context, path string) (io.ReadCloser, error)
}

// FS 是联邦卷 sync.FS 适配：读方法转发 Reader；写方法转发 Writer（roadmap P2
// 联邦卷回写：装配层注入 remote.Client.FS(ref) 含写面），未注入 Writer 恒
// ErrReadOnly（零回归 fail-closed）。
type FS struct {
	r    Reader
	w    syncpkg.FS
	mode ConflictMode // 写冲突策略；默认 lww（WithConflictMode 未调用 = 零回归）
}

// New 构造只读联邦卷适配（r 为注入的远端读实现；nil 拒绝——fail-fast）。
func New(r Reader) (*FS, error) {
	if r == nil {
		return nil, fmt.Errorf("federated: Reader 注入为空（联邦卷需远端读实现）")
	}
	return &FS{r: r}, nil
}

// WithWriter 注入写面（sync.FS 含 WriteFile/Rename/Delete/MakeDir；装配层传
// remote.Client.FS(ref)）。未调用 → 写方法恒 ErrReadOnly。
func (f *FS) WithWriter(w syncpkg.FS) *FS {
	f.w = w
	return f
}

// WithConflictMode 设置写冲突策略（装配层按 extra.conflict_mode 调用；未调用 = lww 零回归）。
// version/conflict 要求写面实现 VersionedWriter——不满足在 WriteFile 时返回
// ErrVersionedUnsupported（fail-closed，禁静默降级回 lww；装配层另有启动期 fail-fast）。
func (f *FS) WithConflictMode(mode ConflictMode) *FS {
	f.mode = mode
	return f
}

// ListDir 列远端卷目录（只读转发）。
func (f *FS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return f.r.ListDir(ctx, path)
}

// Stat 取远端卷路径元信息（只读转发）。
func (f *FS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return f.r.Stat(ctx, path)
}

// OpenRead 打开远端卷读流（只读转发）。
func (f *FS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return f.r.OpenRead(ctx, path)
}

// WriteFile 写文件：注入 Writer 时按冲突策略分派；否则 ErrReadOnly（fail-closed）。
//
//   - lww（默认）：直转 Writer（现状零回归，无版本检查）。
//   - version：CheckVersion 取当前版本 → WriteFileVersioned(expected=当前) 远端 CAS；
//     远端版本 != expected → VersionConflictError（409 + 当前版本号）。
//   - conflict：CheckVersion 发现远端已存在 → 旧文件改名 `<path>.conflict-<ts>` 保留双方
//     （sync 既有惯例命名，冲突文件进用户卷视图可被冲突索引/API 消费），再以
//     「期望不存在」写新内容；rename 失败 → 500（不写新内容，防丢旧内容）。
func (f *FS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	if f.w == nil {
		return ErrReadOnly
	}
	switch f.mode {
	case ConflictVersion:
		vw, ok := f.w.(VersionedWriter)
		if !ok {
			return fmt.Errorf("%w（path=%s）", ErrVersionedUnsupported, path)
		}
		cur, err := vw.CheckVersion(ctx, path)
		if err != nil {
			return fmt.Errorf("federated: 版本检查失败 %s: %w", path, err)
		}
		return vw.WriteFileVersioned(ctx, path, r, size, mtime, cur)
	case ConflictConflict:
		vw, ok := f.w.(VersionedWriter)
		if !ok {
			return fmt.Errorf("%w（path=%s）", ErrVersionedUnsupported, path)
		}
		cur, err := vw.CheckVersion(ctx, path)
		if err != nil {
			return fmt.Errorf("federated: 版本检查失败 %s: %w", path, err)
		}
		if cur >= 0 {
			// 远端已存在 → 冲突：旧文件原子改名 .conflict-<ts> 保留旧内容（防静默丢失），
			// 再以「期望不存在」写新内容（rename 后原路径已空；期间被并发创建则 CAS 409）。
			conflictName := fmt.Sprintf("%s.conflict-%d", path, time.Now().UnixNano())
			if err := vw.Rename(ctx, path, conflictName); err != nil {
				return fmt.Errorf("federated: 冲突旧文件改名失败 %s → %s: %w", path, conflictName, err)
			}
		}
		return vw.WriteFileVersioned(ctx, path, r, size, mtime, -1)
	default: // ConflictLWW：直转，零回归
		return f.w.WriteFile(ctx, path, r, size, mtime)
	}
}

// Rename 联邦卷只读：恒 ErrReadOnly。
func (f *FS) Rename(ctx context.Context, from, to string) error {
	if f.w == nil {
		return ErrReadOnly
	}
	return f.w.Rename(ctx, from, to)
}

// Delete 联邦卷只读：恒 ErrReadOnly。
func (f *FS) Delete(ctx context.Context, path string) error {
	if f.w == nil {
		return ErrReadOnly
	}
	return f.w.Delete(ctx, path)
}

// MakeDir 联邦卷只读：恒 ErrReadOnly。
func (f *FS) MakeDir(ctx context.Context, path string) error {
	if f.w == nil {
		return ErrReadOnly
	}
	return f.w.MakeDir(ctx, path)
}
