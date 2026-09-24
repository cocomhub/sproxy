// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
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
		if e.IsDir() {
			if strings.HasPrefix(e.Name(), ".__") {
				continue
			}
			child := filepath.ToSlash(filepath.Join(rel, e.Name()))
			d.Dirs++
			if err := walkDuDir(root, child, d); err != nil {
				return err
			}
			continue
		}
		if e.Name() == "checksums.json" || e.Name() == "LAYOUT_VERSION" {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			return ierr
		}
		d.Files++
		d.Size += info.Size()
	}
	return nil
}
