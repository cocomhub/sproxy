// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

// remote_block.go 实现写面块级会话端点（roadmap 4.3 P2 跨 FS 块级 v2）：
// A 侧引擎对远端目标做块级差异写时经写面链路调 open/write/close。
// 授权复用 write 面 authorize（同一白名单面）；块语义委派 files.BlockWrite 会话。

// serveBlock 统一走「授权 → 委派 → 审计」三步（与写面 serve 同构）。
func (wh *remoteWriteHandler) serveBlock(w http.ResponseWriter, r *http.Request, op string) {
	tgt, _, ok := wh.authorize(w, r, "write")
	if !ok {
		return
	}
	svc := wh.h.fileService()
	switch op {
	case "block_open":
		size, _ := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
		mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
		id, err := svc.OpenBlockWrite(r.Context(), tgt.owner, tgt.path, tgt.vol.Name, size, mtime)
		if err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		sendJSONResponse(w, map[string]string{"block_id": id}, http.StatusOK)
	case "block_write":
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBlockWriteBytes))
		if err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		if err := svc.WriteBlock(r.Context(), id, offset, body); err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		sendJSONResponse(w, map[string]string{"ok": "written"}, http.StatusOK)
	case "block_close":
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if err := svc.CloseBlockWrite(r.Context(), id); err != nil {
			writeRemoteFilesError(w, err)
			return
		}
		sendJSONResponse(w, map[string]string{"ok": "closed"}, http.StatusOK)
	}
}

// maxBlockWriteBytes 是单块写 body 上限（默认块 1 MiB；留余量）。
const maxBlockWriteBytes = 8 << 20
