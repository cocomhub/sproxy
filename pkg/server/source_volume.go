// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// source_volume.go —— 「owner 视图内某文件的源定位」统一入口（本地卷 / 外部卷含加密卷）。
//
// 背景（缺陷修复，2026-10-21）：分享创建/访问只经 `locateOwnerFile`（**本地卷租户根**）
// 定位源文件 ⇒ 位于**外部卷**（含 `type: secretdata` 加密卷、网盘卷、S3 卷）的文件一律
// 404（用户实测：vault 里的文件创建分享返回 404）。同类问题见于 AI 摘要（`aiSample` 只用
// `tenantFor(owner)`）与版本列表的「文件是否可见」兜底判据。
//
// 本文件把「按卷定位 + 打开」收敛为**唯一**实现：
//   - 本地卷 ⇒ 租户根（`storage.Root`，走用户可见入口：透明解密/封印）；
//   - 外部卷 ⇒ 卷 FS（`trusted.Guard(trusted.Wrap(...))`，键经 `externalVolumeKey` 唯一映射）。
//
// 调用方只依赖 `ownerSource`，不感知卷类型（与 `resolveExternalDownload`/转存同一收敛方向）。

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// ownerSource 是 owner 视图内某文件的源定位（本地卷根或外部卷 FS 二选一）。
type ownerSource struct {
	Volume string // 卷名（"" = 无卷语义的旧装配路径）
	// TenantID 是创建者 owner（归一化后的合法段名）；分享/审计沿用该字段做归属与 ACL 复核。
	TenantID string
	// Rel 是**域内路径**（`user/<path>`），分享记录/事件/审计沿用；本地卷直接用于 Root.Open。
	Rel string
	// Local 非 nil ⇒ 本地卷租户根（读经 openUserVisibleRead：透明解密 + 封印判定）。
	Local *storage.Root
	// FS 非 nil ⇒ 外部卷 FS（键 Key；`secretdata` 等加密卷在此层透明解密）。
	FS  syncpkg.FS
	Key string
	// Size 是**用户可见**字节数（加密卷为明文长度；本地卷来自 Stat）。
	Size int64
}

// OpenRead 打开源文件（用户可见语义：加密卷返回明文）。
func (s *ownerSource) OpenRead(ctx context.Context) (io.ReadCloser, error) {
	if s == nil {
		return nil, fmt.Errorf("server: nil owner source")
	}
	if s.Local != nil {
		return openUserVisibleRead(s.Local, s.Rel)
	}
	if s.FS == nil {
		return nil, fmt.Errorf("server: 源定位缺少 FS（卷 %q）", s.Volume)
	}
	return s.FS.OpenRead(ctx, s.Key)
}

// resolveOwnerSource 解析 (owner, rel) 的源定位。
//
//   - `hint` 非空 ⇒ **只在该卷**定位（显式 `?volume=`/请求体 `volume`；ACL 不通过或文件
//     不存在 ⇒ 未命中，绝不回落到其它卷）。
//   - `hint` 为空 ⇒ 本地卷优先（`locateOwnerFile`：默认卷快路径 + 视图内本地卷扫描），
//     未命中再扫描 owner 视图内的**外部卷**（加密卷/网盘卷/S3…）。
//     多卷同 rel 属歧义，调用方应显式传卷（本函数取首个命中，与 `locateOwnerFile` 的
//     「至多一卷命中」不变式一致）。
//
// rel 是域内路径（`user/<path>`）。返回 false 表示未命中（不区分子类原因，调用方按 404）。
func (h *Handlers) resolveOwnerSource(ctx context.Context, owner, rel, hint string) (*ownerSource, bool) {
	if h.volSet == nil {
		return h.resolveOwnerSourceLegacy(owner, rel)
	}
	owner = normalizeOwner(owner)
	if hint != "" {
		v, ok := h.volSet.ByName(hint)
		if !ok || !v.Authorize(owner) {
			return nil, false
		}
		return h.sourceForVolume(ctx, v, owner, rel)
	}
	if src, ok := h.sourceFromLocalView(owner, rel); ok {
		return src, true
	}
	return h.scanExternalVolumes(ctx, owner, rel)
}

// resolveOwnerSourceLegacy 旧装配路径（volSet nil）：唯一根即默认租户，无卷语义。
func (h *Handlers) resolveOwnerSourceLegacy(owner, rel string) (*ownerSource, bool) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, false
	}
	fi, err := tnt.Root().Stat(rel)
	if err != nil || fi.IsDir() {
		return nil, false
	}
	return &ownerSource{TenantID: tnt.ID, Rel: rel, Local: tnt.Root(), Size: fi.Size()}, true
}

// sourceFromLocalView 在**本地卷视图**内定位（默认卷快路径 + 其余本地卷扫描）。
func (h *Handlers) sourceFromLocalView(owner, rel string) (*ownerSource, bool) {
	loc, ok := h.locateOwnerFile(owner, rel)
	if !ok || loc == nil || loc.tenant == nil || loc.tenant.Root() == nil {
		return nil, false
	}
	fi, err := loc.tenant.Root().Stat(rel)
	if err != nil || fi.IsDir() {
		return nil, false
	}
	return &ownerSource{
		Volume: loc.volumeName, TenantID: loc.tenant.ID, Rel: rel,
		Local: loc.tenant.Root(), Size: fi.Size(),
	}, true
}

// scanExternalVolumes 遍历 owner 视图内的**外部卷**（加密卷/网盘卷/S3）探测 rel。
func (h *Handlers) scanExternalVolumes(ctx context.Context, owner, rel string) (*ownerSource, bool) {
	for _, v := range volume.AllowedVolumes(h.volSet.All(), owner) {
		if h.volSet.HasLocalRoot(v.Name) {
			continue
		}
		if src, ok := h.sourceForVolume(ctx, v, owner, rel); ok {
			return src, true
		}
	}
	return nil, false
}

// sourceForVolume 在**指定卷**上定位文件（本地卷 ⇒ 租户根；外部卷 ⇒ 卷 FS 键）。
// ACL 已由调用方校验（此处不重复 Authorize，但 egress 卷额外做 holder_owner 归属校验，
// 与 `resolveExternalDownload` 同口径：否则任一本地 owner 可经出口卷读持有侧数据）。
func (h *Handlers) sourceForVolume(ctx context.Context, v volume.Volume, owner, rel string) (*ownerSource, bool) {
	if h.volSet.HasLocalRoot(v.Name) {
		tnt := h.volumeTenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			return nil, false
		}
		fi, err := tnt.Root().Stat(rel)
		if err != nil || fi.IsDir() {
			return nil, false
		}
		// 只接受 user 桶（防止分享/摘要面经 rel 读其它功能桶）。
		if !strings.HasPrefix(rel, tnt.UserRoot()+"/") {
			return nil, false
		}
		return &ownerSource{Volume: v.Name, TenantID: tnt.ID, Rel: rel, Local: tnt.Root(), Size: fi.Size()}, true
	}

	be := h.volSet.External(v.Name)
	if be == nil {
		return nil, false
	}
	raw := be.FS()
	if raw == nil {
		return nil, false
	}
	if !egressOwnerMatch(v, owner) {
		return nil, false
	}
	fsys := trusted.Guard(trusted.Wrap(raw, h.trustedWrapOpts()))
	key, ok := externalVolumeKey(v, owner, strings.TrimPrefix(rel, "user/"))
	if !ok {
		return nil, false
	}
	e, err := fsys.Stat(ctx, key)
	if err != nil || e == nil || e.IsDir {
		return nil, false
	}
	return &ownerSource{Volume: v.Name, TenantID: normalizeOwner(owner), Rel: rel, FS: fsys, Key: key, Size: e.Size}, true
}
