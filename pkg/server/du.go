// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// duResponse 是 GET /api/du 的响应体。
// data 为空表示成功但路径下无条目（或路径本身即空目录）；遍历 IO 错误返回
// success:false（fail-closed——不得把部分结果当完整结果）。
type duResponse struct {
	Success bool    `json:"success"`
	Message string  `json:"message,omitempty"`
	Data    *duData `json:"data,omitempty"`
}

// duData 是单个目录的递归统计结果。
type duData struct {
	Path  string `json:"path"`
	Dirs  int    `json:"dirs"`
	Files int    `json:"files"`
	Size  int64  `json:"size"`
}

// duHandler 处理 GET /api/du?path=<rel>[&volume=<name>]：按目录递归统计
// （子目录数/文件数/字节）。owner 从认证派生；卷定位走 locateForRead
// （ACL 收口，未命中统一 404，不泄卷存在性）。path 缺省 = 当前卷根（user 桶）。
func (h *Handlers) duHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ActorFrom(r.Context()))
	tnt0 := h.tenantFor(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	remotePath := r.URL.Query().Get("path")
	// **卷 URI 语义**（用户口径 2026-10-14）：以 `/` 开头 = **卷根起算**（`du <卷>:/a/b`）⇒ 去掉前导
	// 斜杠后按 user 桶相对路径解析；空/`/` = 卷根。此前绝对路径被 `ValidateFilePath` 拒绝 ⇒
	// `du <卷>:/` 恒 400（实测）。
	remotePath = strings.TrimPrefix(strings.TrimSpace(remotePath), "/")
	var rel string
	if remotePath == "" {
		rel = tnt0.UserRoot()
	} else {
		if _, err := pathguard.ValidateFilePath(remotePath); err != nil {
			sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
			return
		}
		var ok bool
		rel, ok = tnt0.UserRel(remotePath)
		if !ok {
			sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
			return
		}
	}

	// **外部卷**（加密/远端卷：无本地 Root）：走**外部 FS** 递归统计。此前恒 404——`locateForRead`
	// 需要本地租户，而外部卷没有 ⇒ `du <外部卷>:/…` 不可用（实测）。
	if explicitVolEarly := r.URL.Query().Get("volume"); explicitVolEarly != "" {
		if v, ok := h.volSet.ByName(explicitVolEarly); ok && !h.volSet.HasLocalRoot(explicitVolEarly) {
			h.duExternalVolume(w, r, owner, v, remotePath)
			return
		}
	}

	// 卷定位（ACL 收口）：显式 ?volume= 只在指定卷定位；未命中 → 404
	// （与 download 一致，不泄卷存在性）。全视图未命中时仅当默认卷对
	// owner 授权才回落默认租户（与下载回落语义一致）。
	explicitVol := r.URL.Query().Get("volume")
	tnt := tnt0
	loc, found := h.locateForRead(owner, rel, explicitVol)
	switch {
	case found:
		tnt = loc.tenant
	case explicitVol != "", !h.defaultVolumeAllows(owner):
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	default:
		// 全视图未命中（默认卷回落）：仍以默认租户根 stat 探测路径存在性——
		// 不存在 → 404（与 download 回落语义一致，不泄卷存在性）。
		if _, serr := tnt.Root().Stat(rel); serr != nil {
			sendJSONResponse(w, duResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
			return
		}
	}

	data, werr := walkDu(tnt.Root(), rel)
	if werr != nil {
		h.log().WarnContext(r.Context(), "du: 遍历目录失败", "path", rel, "volume", tnt.ID, "error", werr)
		sendJSONResponse(w, duResponse{Success: false, Message: "遍历目录失败"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, duResponse{Success: true, Data: data}, http.StatusOK)
}

// duExternalVolume 统计**外部卷**（无本地 Root）子树：经同一可信链取外部 FS，用 `externalVolumeKey`
// 把用户可见路径翻译为卷内键（与 ListDirView/ExternalStat 同一解析），再递归列举统计。
// 语义与本地 `walkDu` 对齐（跳过 .__ 魔法目录与 checksums.json/LAYOUT_VERSION 元数据）。
func (h *Handlers) duExternalVolume(w http.ResponseWriter, r *http.Request, owner string, v volume.Volume, remotePath string) {
	fsys, ok := h.externalFSFor(v.Name)
	if !ok || !v.Authorize(owner) {
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	// 用户可见路径 → 卷内键：`ResolveUserLocation` 是**唯一入口**（含路径安全校验；空路径 = user 桶根）。
	loc, kerr := v.ResolveUserLocation(owner, remotePath)
	if kerr != nil {
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	key := loc.FSPath()
	data, werr := walkDuFS(r.Context(), fsys, key)
	if werr != nil {
		h.log().WarnContext(r.Context(), "du: 遍历外部卷失败", "volume", v.Name, "path", remotePath, "error", werr)
		sendJSONResponse(w, duResponse{Success: false, Message: "遍历目录失败"}, http.StatusInternalServerError)
		return
	}
	data.Path = "/" + strings.TrimPrefix(remotePath, "/")
	sendJSONResponse(w, duResponse{Success: true, Data: data}, http.StatusOK)
}

// walkDuFS 递归统计外部 FS 上 key 子树的目录数/文件数/字节（与 walkDu 同语义）。
func walkDuFS(ctx context.Context, fsys syncpkg.FS, key string) (*duData, error) {
	d := &duData{}
	if werr := walkDuFSDir(ctx, fsys, key, d); werr != nil {
		return nil, werr
	}
	return d, nil
}

// walkDuFSDir 递归一个目录层（目录不存在 = 空，与本地 walkDuDir 一致）。
func walkDuFSDir(ctx context.Context, fsys syncpkg.FS, key string, d *duData) error {
	entries, lerr := fsys.ListDir(ctx, key)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			return nil
		}
		return lerr
	}
	for _, e := range entries {
		if werr := walkDuFSEntry(ctx, fsys, key, e, d); werr != nil {
			return werr
		}
	}
	return nil
}

// walkDuFSEntry 统计单个条目（目录递归并跳过 `.__` 魔法目录；文件跳过元数据文件）。
func walkDuFSEntry(ctx context.Context, fsys syncpkg.FS, key string, e syncpkg.Entry, d *duData) error {
	if e.IsDir {
		if strings.HasPrefix(e.Name, ".__") {
			return nil
		}
		d.Dirs++
		return walkDuFSDir(ctx, fsys, path.Join(key, e.Name), d)
	}
	if e.Name == "checksums.json" || e.Name == "LAYOUT_VERSION" {
		return nil
	}
	d.Files++
	d.Size += e.Size
	return nil
}

// walkDu 递归统计 root 下 rel 子树（rel 为 root 相对路径，含 user/ 前缀）。
// 返回统计值；任何 WalkDir/ReadDir 错误 fail-closed（返回错误，不交付部分结果）。
// 语义对齐 stats 桶遍历：跳过 .__ 魔法目录与 checksums.json/LAYOUT_VERSION 元数据。
func walkDu(root *storage.Root, rel string) (*duData, error) {
	d := &duData{Path: rel}
	err := walkDuDir(root, rel, d)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func walkDuDir(root *storage.Root, rel string, d *duData) error {
	entries, err := root.ReadDir(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := walkDuEntry(root, rel, e, d); err != nil {
			return err
		}
	}
	return nil
}

// walkDuEntry 统计单个目录条目：目录递归（跳过 .__ 魔法目录）；文件计入
// （跳过 checksums.json / LAYOUT_VERSION 元数据）。语义对齐 stats 桶遍历。
func walkDuEntry(root *storage.Root, rel string, e os.DirEntry, d *duData) error {
	if e.IsDir() {
		if strings.HasPrefix(e.Name(), ".__") {
			return nil
		}
		child := filepath.ToSlash(filepath.Join(rel, e.Name()))
		d.Dirs++
		return walkDuDir(root, child, d)
	}
	if e.Name() == "checksums.json" || e.Name() == "LAYOUT_VERSION" {
		return nil
	}
	info, ierr := e.Info()
	if ierr != nil {
		return ierr
	}
	d.Files++
	d.Size += info.Size()
	return nil
}
