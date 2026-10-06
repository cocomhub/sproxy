// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cocomhub/sproxy/pkg/volume"
)

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

// register 登记一条占用关联。重复登记（同 wrapper 已占）→ false（不覆盖）。
func (r *volumeLinksRegistry) register(l volumeLink) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.links {
		if e.Wrapper == l.Wrapper && e.Base == l.Base && e.Subdir == l.Subdir {
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

// links 返回嵌套封装关联索引（懒创建 + 装配期一次性从用户卷 store 重建）。
// 重建防重启丢失：扫描全部用户卷中 target 为 `<卷>/<子目录>` 形态的封装卷并登记。
func (h *Handlers) links() *volumeLinksRegistry {
	if h.volumeLinks == nil {
		h.volumeLinks = newVolumeLinksRegistry()
	}
	h.rebuildLinksOnce.Do(func() {
		if h.userVolumes == nil {
			return
		}
		vols, err := h.userVolumes.ScanRestore()
		if err != nil {
			return // 扫描失败不阻断（后续运行时创建会重新登记）
		}
		for _, v := range vols {
			base, subdir, ok := volume.SplitNestedTarget(extraTargetStr(v.Extra))
			if !ok {
				continue
			}
			h.volumeLinks.register(volumeLink{Base: base, Subdir: subdir, Wrapper: v.Name, Owner: v.Owner})
		}
	})
	return h.volumeLinks
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
// （仅嵌套 target）。关联生命周期：删底层卷查引用 409、删封装卷清关联。
func (h *Handlers) registerWrapperLink(owner, wrapper string, extra map[string]any) {
	base, subdir, ok := volume.SplitNestedTarget(extraTargetStr(extra))
	if !ok {
		return
	}
	h.links().register(volumeLink{Base: base, Subdir: subdir, Wrapper: wrapper, Owner: owner})
}

// clearWrapperLinks 在封装卷删除成功后清除其占用关联。
func (h *Handlers) clearWrapperLinks(wrapper string) {
	h.links().unregisterByWrapper(wrapper)
}
