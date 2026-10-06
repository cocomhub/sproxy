// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// 旁路写面占用检查（用户语义 #6 写保护，2026-10-06 闭环）：
// files 域（上传/分块 complete/删除/重命名/mkdir/rmdir）+ 版本 restore 已接 guard；
// 云转存（pkg/cloud transfer）/跨卷 move-rebalance / S3 / WebDAV / sync / 备份恢复等
// 直写卷根的旁路面，各自接入 checkWrapperOccupiedWrite（同坐标判定），否则「只读」语义
// 对加密数据无实际保护。读路径一律不拦。

// volume_links.go 是嵌套封装（秘密用「已创建卷 + 新子目录」作为底层根）的**互斥占用 +
// 关联标注**模型（用户 2026-10-06 确认的最终语义）：
//
//   - 互斥占用：底层卷某目录被封装卷占用后，该目录及父/子目录（路径重叠，volume.pathsOverlap）
//     均不可再被其它封装卷选作底层 → 建卷校验 409。同卷不同分支、跨卷互不影响。
//   - 关联标注：建封装卷登记「底层卷 → {subdir, wrapper}」；删底层卷有引用 → 409；
//     删封装卷清关联。防重启丢失由装配层重建（见 rebuildFromUserVolumes）。
//
// 写保护（被占用目录只读可查、禁止修改）由 pkg/sync.ReadonlySubFS 承担（本包不必 import）。

// errVolumeDirOccupied 是互斥占用冲突哨兵错误（建卷校验返回，409）。
var errVolumeDirOccupied = errors.New("volume: 底层卷子目录已被封装卷占用")

// errVolumeOccupiedReadOnly 是普通写路径对「被封装卷占用的底层子目录」的写保护拦截哨兵错误
// （用户语义 #6：原始底层卷该子目录禁止写、只能读；封装卷自身不受影响——不同卷不冲突）。
// 普通文件写操作（上传/分块完成/删除/批量删除/重命名/mkdir/rmdir/版本 restore）命中 → 403。
var errVolumeOccupiedReadOnly = errors.New("volume: 目录已被封装卷占用，只读")

// errVolumeDirExists 是嵌套封装子目录已存在哨兵错误（建卷校验返回，409「目录已存在」，
// 防与底层卷既有数据混合）。
var errVolumeDirExists = errors.New("volume: 目录已存在（需 <卷>/<新子目录>，子目录须当前不存在）")

// volumeLink 记录一次「封装卷占用底层卷某子目录」的关联。
type volumeLink struct {
	Base    string // 底层卷名
	Subdir  string // 被占用子目录（相对底层卷根）
	Wrapper string // 封装卷名
	Owner   string // 封装卷 owner（审计/关联生命周期）
}

// volumeLinksRegistry 是封装关联的互斥索引：按底层卷聚合，串行化读写。
type volumeLinksRegistry struct {
	mu    sync.Mutex
	links []volumeLink
}

// newVolumeLinksRegistry 构造空关联索引。
func newVolumeLinksRegistry() *volumeLinksRegistry {
	return &volumeLinksRegistry{}
}

// register 登记一条占用关联。重复登记（同 wrapper 已占）→ false（不覆盖）；
// 与既有占用**路径重叠**（互斥占用，volume.PathsOverlap）→ false（拒绝——防并发建卷
// 重叠子目录的 TOCTOU：validate 预检与 register 登记之间无跨请求锁，register 内再做
// 一次互斥判定 fail-closed，后到者回滚）。
func (r *volumeLinksRegistry) register(l volumeLink) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.links {
		if e.Wrapper == l.Wrapper && e.Base == l.Base && e.Subdir == l.Subdir {
			return false
		}
		if e.Base == l.Base && volume.PathsOverlap(e.Subdir, l.Subdir) {
			return false
		}
	}
	r.links = append(r.links, l)
	return true
}

// unregisterByWrapper 清除某封装卷的全部占用关联（删封装卷时）。返回清除条数。
func (r *volumeLinksRegistry) unregisterByWrapper(wrapper string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.links[:0]
	removed := 0
	for _, e := range r.links {
		if e.Wrapper == wrapper {
			removed++
			continue
		}
		out = append(out, e)
	}
	r.links = out
	return removed
}

// refsOfBase 返回以 base 为底层卷的全部关联（删底层卷前查引用 → 409）。
func (r *volumeLinksRegistry) refsOfBase(base string) []volumeLink {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []volumeLink
	for _, e := range r.links {
		if e.Base == base {
			out = append(out, e)
		}
	}
	return out
}

// conflicts 判定是否已有封装卷占用了 base 卷的 subdir（或与其路径重叠）：
// 命中 → 返回 (已有占用, true)；否则 (零值, false)。互斥规则 = pathsOverlap。
func (r *volumeLinksRegistry) conflicts(base, subdir string) (volumeLink, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.links {
		if e.Base != base {
			continue
		}
		if volume.PathsOverlap(e.Subdir, subdir) {
			return e, true
		}
	}
	return volumeLink{}, false
}

// hasRefs 底层卷是否被任何封装卷引用（删底层卷 → 409）。
func (r *volumeLinksRegistry) hasRefs(base string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.links {
		if e.Base == base {
			return true
		}
	}
	return false
}

// links 返回嵌套封装关联索引（懒创建 + 装配期从用户卷 store 重建）。
// 重建防重启丢失：扫描全部用户卷中 target 为 `<卷>/<子目录>` 形态的封装卷并登记。
//
// 并发安全：注册表**创建也纳入 rebuildLinksOnce**（同一 sync.Once 内先建——避免
// `if h.volumeLinks == nil` 裸检查在并发首次访问（上传路由 guard / 建卷校验等）下对指针的
// 数据竞争）。重建扫描是**每次调用幂等重试**（tryRestoreLinks，restored 原子标记）：
//   - store 未装配（userVolumes nil，进程启动竞序）→ 本次只建空表，不标记；store Set 后
//     下次调用再恢复（不再因 Once 首次空重建缓存而永久丢失已持久化占用）；
//   - ScanRestore 失败（读盘错误）→ 不标记，下次调用重试（救回旧占用，非仅补新 wrapper）。
func (h *Handlers) links() *volumeLinksRegistry {
	h.rebuildLinksOnce.Do(func() {
		h.volumeLinks = newVolumeLinksRegistry()
	})
	h.tryRestoreLinks()
	return h.volumeLinks
}

// tryRestoreLinks 幂等地从用户卷 store 重建占用关联（见 links 头注释；原子标记幂等）。
func (h *Handlers) tryRestoreLinks() {
	if h.linksRestored.Load() {
		return
	}
	if h.userVolumes == nil {
		return // store 未装配（启动竞序）：链表空、不标记，待 Set 后下次 links() 调用恢复。
	}
	h.restoreLinksMu.Lock()
	defer h.restoreLinksMu.Unlock()
	if h.linksRestored.Load() {
		return
	}
	vols, err := h.userVolumes.ScanRestore()
	if err != nil {
		return // 扫描失败不标记，下次 links() 调用重试（不因 Once 丢失旧占用恢复）。
	}
	for _, v := range vols {
		base, subdir, ok := volume.SplitNestedTarget(extraTargetStr(v.Extra))
		if !ok {
			continue
		}
		h.volumeLinks.register(volumeLink{Base: base, Subdir: subdir, Wrapper: v.Name, Owner: v.Owner})
	}
	h.linksRestored.Store(true)
}

// extraTargetStr 从卷 Extra 读 target 字段（统一 nil 安全）。
func extraTargetStr(extra map[string]any) string {
	if extra == nil {
		return ""
	}
	s, _ := extra["target"].(string)
	return s
}

// validateNestedWrapperTarget 校验嵌套封装 target（`<卷>/<子目录>`，建卷校验、创建前调用）：
//
//   - 互斥占用：底层卷该子目录（或与其路径重叠的目录）已被其它封装卷占用 → errVolumeDirOccupied；
//   - 子目录必须不存在：Stat 确认当前不存在 → 存在则 errVolumeDirExists（防与既有数据混合）。
//
// 仅 target 命中 SplitNestedTarget 时执行；非嵌套 target（local+root / 外部 target 预留）零回归。
// 不创建目录、不登记占用——MakeDir 由 backend 工厂（resolveTargetFS）在 NewBackend 时执行，
// 登记由 createUserVolumeHandler 在 AddExternalVolume 成功后执行（创建后登记）。
func (h *Handlers) validateNestedWrapperTarget(ctx context.Context, extra map[string]any) error {
	base, subdir, ok := volume.SplitNestedTarget(extraTargetStr(extra))
	if !ok {
		return nil
	}
	if _, hit := h.links().conflicts(base, subdir); hit {
		return errVolumeDirOccupied
	}
	fs, ok2 := h.volSet.FSFor(base)
	if !ok2 {
		return fmt.Errorf("volume: 底层卷 %q 不可用", base)
	}
	e, serr := fs.Stat(ctx, subdir)
	if serr != nil {
		return fmt.Errorf("volume: 检查底层子目录 %s/%s 失败: %w", base, subdir, serr)
	}
	if e != nil {
		return errVolumeDirExists
	}
	return nil
}

// registerWrapperLink 在封装卷创建成功后登记「底层卷 → {subdir, wrapper}」占用关联
// （仅嵌套 target）。互斥占用冲突（register 内重叠判定，防并发建卷 TOCTOU）→ false；
// 非嵌套 target 视为成功（无关联可登记）。调用方据返回值回滚已建卷。
func (h *Handlers) registerWrapperLink(owner, wrapper string, extra map[string]any) bool {
	base, subdir, ok := volume.SplitNestedTarget(extraTargetStr(extra))
	if !ok {
		return true // 非嵌套 target：无关联可登记（视为成功）。
	}
	return h.links().register(volumeLink{Base: base, Subdir: subdir, Wrapper: wrapper, Owner: owner})
}

// userVisibleRelOf 把租户根内相对路径（user/<path>）归一为用户可见相对路径（<path>），
// 与占用 Subdir（卷内用户可见坐标，如 videos，不含 user/<owner> 前缀）对齐比较。
func userVisibleRelOf(rel string) string {
	return strings.TrimPrefix(rel, "user/")
}

// checkWrapperOccupiedWrite 判定 baseVolName 卷上 userVisibleRel（相对用户桶的可见路径，如
// videos/x）的写是否命中被封装卷占用的子目录（用户语义 #6 写保护：原始底层卷该子目录禁止写、
// 只能读）。命中 → errVolumeOccupiedReadOnly；未命中 → nil。
//
// baseVolName 为空（无卷语义旧装配回落默认租户）时归一为默认卷名（占用在默认卷时同样拦截）。
// 坐标说明：占用 Subdir 是卷根相对的用户可见坐标（如 main/videos → videos）；传入的
// userVisibleRel 同为该坐标（调用方用 userVisibleRelOf(rel) 归一）。比较用定向包含
// （relPath == subdir || HasPrefix(relPath, subdir+"/")），即「写目标落在占用子目录内/自身」
// → 拦截；写父目录/兄弟路径不拦（与互斥占用的对称 PathsOverlap 区分——写保护是定向的）。
func (h *Handlers) checkWrapperOccupiedWrite(baseVolName, userVisibleRel string) error {
	if baseVolName == "" && h.volSet != nil {
		baseVolName = h.volSet.Default().Name
	}
	for _, l := range h.links().refsOfBase(baseVolName) {
		if userVisibleRel == l.Subdir || strings.HasPrefix(userVisibleRel, l.Subdir+"/") {
			return fmt.Errorf("%w（%s/%s 被封装卷 %s 占用）", errVolumeOccupiedReadOnly, baseVolName, l.Subdir, l.Wrapper)
		}
	}
	return nil
}

// clearWrapperLinks 在封装卷删除成功后清除其占用关联。
func (h *Handlers) clearWrapperLinks(wrapper string) {
	h.links().unregisterByWrapper(wrapper)
}

// defaultVolumeName 返回默认卷名（volSet 未装配 → ""；checkWrapperOccupiedWrite 对空名
// 自行回落默认卷语义）。供默认卷写面（sync 等）绑定占用检查。
func (h *Handlers) defaultVolumeName() string {
	if h.volSet != nil {
		if def := h.volSet.Default(); def.Name != "" {
			return def.Name
		}
	}
	return ""
}

// occupiedGuardFS 是 sync.FS 装饰器：写方法（WriteFile/MakeDir/Rename 目标/Delete）委托前按
// 「目标卷 + FS 根相对路径」做占用写保护判定。FS 根即卷 user 桶 → 方法 path 即用户可见坐标
// （与 checkWrapperOccupiedWrite 的 userVisibleRel 一致）。读方法（ListDir/Stat/OpenRead）
// 透传不拦。check 为 nil 或 volName 空 → 直写（零回归）。供 WebDAV / 备份恢复 / sync 写面
// 等「直写卷 root、不经 files 域 guard」的旁路写面统一接入。
type occupiedGuardFS struct {
	inner   syncpkg.FS
	volName string
	check   func(volName, userVisibleRel string) error
}

func (g *occupiedGuardFS) guard(rel string) error {
	if g.check == nil || g.volName == "" {
		return nil
	}
	return g.check(g.volName, rel)
}

func (g *occupiedGuardFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return g.inner.ListDir(ctx, p)
}

func (g *occupiedGuardFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return g.inner.Stat(ctx, p)
}

func (g *occupiedGuardFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return g.inner.OpenRead(ctx, p)
}

func (g *occupiedGuardFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	if err := g.guard(p); err != nil {
		return err
	}
	return g.inner.WriteFile(ctx, p, r, size, mtime)
}

func (g *occupiedGuardFS) Rename(ctx context.Context, from, to string) error {
	if err := g.guard(to); err != nil {
		return err
	}
	return g.inner.Rename(ctx, from, to)
}

func (g *occupiedGuardFS) Delete(ctx context.Context, p string) error {
	if err := g.guard(p); err != nil {
		return err
	}
	return g.inner.Delete(ctx, p)
}

func (g *occupiedGuardFS) MakeDir(ctx context.Context, p string) error {
	if err := g.guard(p); err != nil {
		return err
	}
	return g.inner.MakeDir(ctx, p)
}

// _ 编译期断言：occupiedGuardFS 实现 sync.FS。
var _ syncpkg.FS = (*occupiedGuardFS)(nil)

// wrapOccupiedGuard 把「卷 user 桶根」sync.FS 包上写保护装饰器（volName 为目标卷名）。
// volSet 未装配 / check 不可用 → 原样返回（零回归）。
func (h *Handlers) wrapOccupiedGuard(fs syncpkg.FS, volName string) syncpkg.FS {
	if h == nil || h.volSet == nil {
		return fs
	}
	return &occupiedGuardFS{inner: fs, volName: volName, check: h.checkWrapperOccupiedWrite}
}

// DefaultVolumeWriteGuard 返回绑定默认卷的占用写保护判定回调（供装配层注入默认卷写面——
// 同步本地写侧等）。签名 userRel 为用户可见相对路径（如 videos/x）；命中被封装卷占用的
// 底层子目录 → errVolumeOccupiedReadOnly 包装错误。volSet 未装配 → 回调内默认卷名空、
// checkWrapperOccupiedWrite 无引用不命中（直写，零回归）。不如 wrapOccupiedGuard 传卷名
// 灵活，仅用于「确定写默认卷」的旁路面（sync pull 本地端）。
func (h *Handlers) DefaultVolumeWriteGuard() func(userRel string) error {
	return func(userRel string) error {
		return h.checkWrapperOccupiedWrite(h.defaultVolumeName(), userRel)
	}
}
