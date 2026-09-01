// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// validateChunkChecksum 校验 chunk_checksum 是否为有效的 64 位 hex 字符串。
// 使用 hex.DecodeString + 长度检查实现，一次调用即可完成验证。
func validateChunkChecksum(checksum string) bool {
	if len(checksum) != 64 {
		return false
	}
	_, err := hex.DecodeString(checksum)
	return err == nil
}

// chunkOverheadMargin 是 multipart 表单开销的预估余量，超过此值的 chunk 会被服务端裁剪。
// 客户端 chunk 实际数据 + 此余量必须 ≤ DefaultChunkBodyLimit。
const chunkOverheadMargin = 4 * 1024 // 4 KiB

// negotiateChunkSize 协商分块大小：使用客户端传入的值，但不超过服务端上限。
func negotiateChunkSize(clientChunkSize, cfgChunkSize int64) (chunkSize int64, adjusted bool) {
	chunkSize = clientChunkSize
	if chunkSize <= 0 {
		chunkSize = cfgChunkSize
	}
	if chunkSize <= 0 {
		chunkSize = size.DefaultChunkSize
	}
	if chunkSize > size.DefaultChunkBodyLimit-chunkOverheadMargin {
		chunkSize = size.DefaultChunkBodyLimit - chunkOverheadMargin
		adjusted = true
	}
	return chunkSize, adjusted
}

// checkExistingFileForInit 检查目标文件（租户 user 桶内）是否已存在。
// tnt 非 nil、rel 为租户根内 user 桶相对路径（uploadInit 已解析）。返回 true 表示已处理（调用方应 return）。
func (h *Handlers) checkExistingFileForInit(w http.ResponseWriter, tnt *storage.Tenant, rel, filename, fileChecksum string) bool {
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return true
	}
	root := tnt.Root()
	stat, err := root.Stat(rel)
	if err != nil {
		return false // 文件不存在，继续正常流程
	}
	if verifyFileWithChecksumRoot(root, rel, fileChecksum) {
		h.logger.Info("文件已存在，跳过上传", "file_name", filename, "size", stat.Size(), "checksum", shortid.ShortHash(fileChecksum))
		sendJSONResponse(w, ChunkedInitResponse{
			Success:  true,
			UploadID: "already_exists",
			Message:  fmt.Sprintf(errFmtFileExists, stat.Size()),
		}, http.StatusOK)
		return true
	}
	// 文件存在但 checksum 不匹配，不允许覆盖
	h.logger.Warn("同名文件已存在但 checksum 不匹配", "file_name", filename)
	sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "同名文件已存在但 checksum 不匹配"}, http.StatusConflict)
	return true
}

// parseChunkFormParams 解析分块上传请求中的表单参数。
func parseChunkFormParams(r *http.Request) (uploadID string, chunkIndex int, chunkChecksum string, ok bool) {
	uploadID = r.FormValue("upload_id")
	chunkIndexStr := r.FormValue("chunk_index")
	chunkChecksum = r.FormValue("chunk_checksum")

	if uploadID == "" || chunkIndexStr == "" {
		return "", 0, "", false
	}
	if !validateChunkChecksum(chunkChecksum) {
		return "", 0, "", false
	}
	if _, err := fmt.Sscanf(chunkIndexStr, "%d", &chunkIndex); err != nil {
		return "", 0, "", false
	}
	return uploadID, chunkIndex, chunkChecksum, true
}

// uploadInit 初始化一个分块上传会话。
func (h *Handlers) uploadInit(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	// 限制请求体大小
	r.Body = http.MaxBytesReader(w, r.Body, size.MultipartBufSize) // 1MB 足够
	var req struct {
		UploadID     string `json:"upload_id"`
		Filename     string `json:"filename"`
		TotalSize    int64  `json:"total_size"`
		ChunkSize    int64  `json:"chunk_size"`
		TotalChunks  int    `json:"total_chunks"`
		FileChecksum string `json:"file_checksum"`
		FileModTime  int64  `json:"file_mod_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	h.logger.Debug("uploadInit 请求", "file_name", req.Filename, "total_size", req.TotalSize,
		"chunk_size", req.ChunkSize, "total_chunks", req.TotalChunks,
		"file_checksum", shortid.ShortHash(req.FileChecksum), "upload_id", req.UploadID)

	// 校验字段
	if req.UploadID == "" {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "缺少 upload_id"}, http.StatusBadRequest)
		return
	}
	// upload_id 用裸 id，过 pkg/storage.ValidSegmentName（段名校验单一权威）防路径穿越：
	// 拒绝 / \、".."、".__" 前缀、Windows 非法字符与保留设备名、尾点/尾空格、超长。
	// 租户隔离由 per-tenant chunk 桶物理保证（会话只在本租户 chunk/ 下创建），无需 owner 前缀。
	if !storage.ValidSegmentName(req.UploadID) {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "无效的 upload_id"}, http.StatusBadRequest)
		return
	}
	if _, err := ValidateFilePath(req.Filename); err != nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}
	if req.TotalSize <= 0 {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "total_size 必须大于 0"}, http.StatusBadRequest)
		return
	}
	if req.ChunkSize <= 0 {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "chunk_size 必须大于 0"}, http.StatusBadRequest)
		return
	}
	if req.TotalChunks <= 0 {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "total_chunks 必须大于 0"}, http.StatusBadRequest)
		return
	}
	if req.ChunkSize*int64(req.TotalChunks) < req.TotalSize {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "chunk_size * total_chunks 应 >= total_size"}, http.StatusBadRequest)
		return
	}
	if !validateChunkChecksum(req.FileChecksum) {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "file_checksum 不是有效的 hex"}, http.StatusBadRequest)
		return
	}

	// 取 owner 的租户与 per-tenant UploadStore（会话目录 <root>/<owner>/chunk/<id>/）。
	owner := ownerFromRequest(r)
	store := h.uploadStoreFor(owner)
	if store == nil {
		h.logger.Error("获取 per-tenant UploadStore 失败", "owner", owner)
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
		return
	}
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	rel, ok := tnt.UserRel(req.Filename)
	if !ok {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	// 排他上传检查：同一文件不能并发上传（key 与 upload handler 一致：<tnt.ID>\x00<rel>）
	upKey := tnt.ID + "\x00" + rel
	if _, loaded := h.uploadingFiles.LoadOrStore(upKey, req.UploadID); loaded {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "该文件正在上传中"}, http.StatusConflict)
		return
	}
	defer h.uploadingFiles.Delete(upKey)

	// 已存在同名文件的检查（租户 user 桶）
	if h.checkExistingFileForInit(w, tnt, rel, req.Filename, req.FileChecksum) {
		return
	}

	// 分块大小协商
	chunkSize, adjusted := negotiateChunkSize(req.ChunkSize, cfg.ChunkSize)
	if adjusted {
		h.logger.Info("chunk_size 超出服务端上限，自动裁剪",
			"client_chunk_size", req.ChunkSize,
			"max_chunk_upload_bytes", size.DefaultChunkBodyLimit,
			"file_name", req.Filename,
			"upload_id", shortid.ShortHash(req.UploadID))
		req.TotalChunks = int((req.TotalSize + chunkSize - 1) / chunkSize)
	}

	// 会话直接以裸 id 创建于本租户 store（无 owner 前缀；隔离靠 per-tenant chunk 桶）
	session, reused, err := store.GetOrCreateSession(req.UploadID, req.Filename,
		req.TotalSize, chunkSize, req.TotalChunks, req.FileChecksum, req.FileModTime)
	if err != nil {
		h.logger.Error("创建/续传上传会话失败", "upload_id", req.UploadID, "error", err)
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
		return
	}

	// 预留存储空间（仅新创建会话时预留，续传会话已预留过）。
	// P4 配额接入：优先走租户 Scope（TryReserve），全局兜底由 Scope 父链自动生效；
	// 未装配 quota（scope 不可用）时回退旧 storageMgr 预留（测试/旧装配兼容）。
	if !reused {
		scope := h.quotaFor(owner)
		if scope != nil {
			rr, err := scope.TryReserve(session.TotalSize)
			if err != nil {
				// 预留失败，清理已创建的 session
				store.DeleteSession(session.UploadID)
				h.logger.Warn("storage full, chunked upload rejected",
					"file_name", req.Filename,
					"total_size", session.TotalSize,
				)
				sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "存储空间不足"}, http.StatusInsufficientStorage)
				return
			}
			session.Reservation = rr
		} else if h.storageMgr != nil {
			if err := h.storageMgr.TryReserve(session.TotalSize, CategoryChunked); err != nil {
				// 预留失败，清理已创建的 session
				store.DeleteSession(session.UploadID)
				h.logger.Warn("storage full, chunked upload rejected",
					"file_name", req.Filename,
					"total_size", session.TotalSize,
					"current_usage", h.storageMgr.Usage(),
					"max_bytes", h.storageMgr.MaxBytes(),
				)
				sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "存储空间不足"}, http.StatusInsufficientStorage)
				return
			}
		}
	}

	msg := "上传会话已创建"
	if reused {
		missing := MissingChunks(session)
		msg = fmt.Sprintf("续传会话已恢复，缺失 %d 个分块", len(missing))
		h.logger.Info("续传会话", "upload_id", session.UploadID, "file_name", req.Filename,
			"missing", len(missing), "total", session.TotalChunks)
	} else {
		h.logger.Info("新上传会话", "upload_id", session.UploadID, "file_name", req.Filename,
			"total_size", req.TotalSize, "total_chunks", session.TotalChunks)
	}

	sendJSONResponse(w, ChunkedInitResponse{
		Success:   true,
		UploadID:  session.UploadID,
		ChunkSize: session.ChunkSize,
		Message:   msg,
	}, http.StatusOK)
}

// uploadChunk 上传单个分块。
func (h *Handlers) uploadChunk(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 限制请求体大小（含 multipart 开销）
	r.Body = http.MaxBytesReader(w, r.Body, size.DefaultChunkBodyLimit)

	// 解析 multipart
	if err := r.ParseMultipartForm(size.DefaultChunkBodyLimit); err != nil {
		h.logger.Warn("uploadChunk parse multipart 失败", "error", err.Error(), "content_type", r.Header.Get("Content-Type"), "content_length", r.ContentLength)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "解析 multipart 失败"}, http.StatusRequestEntityTooLarge)
		return
	}
	// I-3：multipart 解析不读到 EOF，读完全部 body 触发 bodyValidator 哈希校验。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	h.logger.Debug("uploadChunk multipart 解析完成", "content_type", r.Header.Get("Content-Type"))

	uploadID, chunkIndex, chunkChecksum, ok := parseChunkFormParams(r)
	if !ok {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "缺少 upload_id、chunk_index 或 chunk_checksum 无效"}, http.StatusBadRequest)
		return
	}

	h.logger.Debug("uploadChunk 请求", "upload_id", uploadID, "chunk_index", chunkIndex, "content_type", r.Header.Get("Content-Type"))

	// 租户隔离靠 per-tenant store：会话只在本租户 chunk/ 桶下创建，跨租户同裸 id 互不可见
	owner := ownerFromRequest(r)
	store := h.uploadStoreFor(owner)
	if store == nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}

	// 获取 session
	session := store.GetSession(uploadID)
	if session == nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}

	if session.Completed {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "上传已完成，不接受新分块"}, http.StatusGone)
		return
	}

	if chunkIndex < 0 || chunkIndex >= session.TotalChunks {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: fmt.Sprintf("chunk_index %d 超出范围 [0, %d)", chunkIndex, session.TotalChunks)}, http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("chunk")
	if err != nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "读取分块文件失败"}, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 幂等：如果该块已接收且 checksum 匹配，直接返回成功
	if session.ReceivedChunks[chunkIndex] && session.ChunkChecksums[chunkIndex] == chunkChecksum {
		h.logger.Debug("chunk 已存在，跳过", "upload_id", uploadID, "chunk_index", chunkIndex, "checksum", shortid.ShortHash(chunkChecksum))
		sendJSONResponse(w, ChunkUploadResponse{Success: true, ChunkIndex: chunkIndex, Message: "分块已存在，跳过"}, http.StatusOK)
		return
	}

	// 获取 chunk 写入路径与IO读锁
	chunkPath := store.ChunkFilePath(uploadID, chunkIndex)
	unlockIO := store.LockChunkIO(uploadID)
	defer unlockIO()

	// 持锁后重新获取 session，用最新副本做幂等检查
	session = store.GetSession(uploadID)
	if session == nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}
	if session.Completed {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "上传已完成，不接受新分块"}, http.StatusGone)
		return
	}
	if chunkIndex < 0 || chunkIndex >= session.TotalChunks {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: fmt.Sprintf("chunk_index %d 超出范围 [0, %d)", chunkIndex, session.TotalChunks)}, http.StatusBadRequest)
		return
	}
	if session.ReceivedChunks[chunkIndex] && session.ChunkChecksums[chunkIndex] == chunkChecksum {
		h.logger.Debug("chunk 已存在，跳过", "upload_id", uploadID, "chunk_index", chunkIndex, "checksum", shortid.ShortHash(chunkChecksum))
		sendJSONResponse(w, ChunkUploadResponse{Success: true, ChunkIndex: chunkIndex, Message: "分块已存在，跳过"}, http.StatusOK)
		return
	}

	// 确保 session 目录存在
	if err = os.MkdirAll(filepath.Dir(chunkPath), 0755); err != nil {
		h.logger.Error("创建 session 目录失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	// 原子写入 + 流式哈希（复用 writeFileAtomically 写临时文件）
	serverChecksum, written, err := writeFileAtomically(r.Context(), chunkPath, file)
	if err != nil {
		h.logger.Error("写入 chunk 失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "写入分块失败"}, http.StatusInternalServerError)
		return
	}

	// 校验 SHA-256
	if serverChecksum != chunkChecksum {
		h.logger.Warn("chunk SHA-256 不匹配", "upload_id", uploadID, "chunk_index", chunkIndex,
			"server", serverChecksum, "server_short", shortid.ShortHash(serverChecksum),
			"client", chunkChecksum, "client_short", shortid.ShortHash(chunkChecksum),
			"written", written, "session_chunk_size", session.ChunkSize)
		sendJSONResponse(w, ChunkUploadResponse{
			Success:     false,
			ChunkIndex:  chunkIndex,
			ShouldRetry: true,
			Message:     "SHA-256 校验不匹配",
		}, http.StatusOK)
		return
	}

	// 更新 session
	if err := store.MarkChunkReceived(uploadID, chunkIndex, serverChecksum); err != nil {
		h.logger.Error("标记分块已接收失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "更新状态失败"}, http.StatusInternalServerError)
		return
	}

	h.logger.Info("uploadChunk 耗时", "upload_id", uploadID, "chunk_index", chunkIndex,
		"total", time.Since(start).String(), "size", written)
	sendJSONResponse(w, ChunkUploadResponse{
		Success:    true,
		ChunkIndex: chunkIndex,
		Message:    fmt.Sprintf("分块 %d 已接收并校验通过", chunkIndex),
	}, http.StatusOK)
}

// uploadSessions 列出所有未完成上传会话的元信息。
// 归一为 {success:true, sessions:[{upload_id,filename,total_size,received_count,total_chunks,file_checksum,file_mod_time,status}]}。
// 已完成会话（Completed=true，complete 后 CleanupSessionAfter 延迟清理前的窗口）不列出，
// 故 status 恒为 "uploading"（取值域 uploading|completed，completed 在此被 handler 过滤）。
func (h *Handlers) uploadSessions(w http.ResponseWriter, r *http.Request) {
	// per-tenant store 的 ListSessions() 天然只含本租户会话，无需 owner 过滤。
	owner := ownerFromRequest(r)
	store := h.uploadStoreFor(owner)
	if store == nil {
		sendJSONResponse(w, ChunkSessionsResponse{Success: true, Sessions: []UploadSessionInfo{}}, http.StatusOK)
		return
	}
	meta := store.ListSessions()
	sessions := make([]UploadSessionInfo, 0, len(meta))
	for _, m := range meta {
		if m.Completed {
			continue
		}
		info := UploadSessionInfo{
			UploadID:      m.UploadID,
			Filename:      m.Filename,
			TotalSize:     m.TotalSize,
			ReceivedCount: m.ReceivedCount,
			TotalChunks:   m.TotalChunks,
			FileChecksum:  m.FileChecksum,
			FileModTime:   m.FileModTime,
			Status:        "uploading",
		}
		sessions = append(sessions, info)
	}
	sendJSONResponse(w, ChunkSessionsResponse{Success: true, Sessions: sessions}, http.StatusOK)
}

// uploadStatus 查询上传会话状态。
func (h *Handlers) uploadStatus(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	uploadID := params.Get("upload_id")
	filename := params.Get("filename")
	owner := ownerFromRequest(r)

	// 1. 按 upload_id 查 session（per-tenant store，天然只含本租户会话）
	if uploadID != "" {
		if h.lookupUploadIDStatus(w, owner, uploadID, filename) {
			return
		}
	}

	// 2. 按 filename 查找未完成的 session（本租户作用域）
	if filename != "" {
		if h.lookupFilenameStatus(w, owner, filename) {
			return
		}
	}

	// 什么都没找到
	sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: "未找到文件或上传会话"}, http.StatusNotFound)
}

// lookupUploadIDStatus 按 upload_id 查询上传会话状态。返回 true 表示已处理请求。
func (h *Handlers) lookupUploadIDStatus(w http.ResponseWriter, owner, uploadID, filename string) bool {
	store := h.uploadStoreFor(owner)
	if store == nil {
		if filename == "" {
			sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
			return true
		}
		return false
	}
	session := store.GetSession(uploadID)
	if session != nil {
		missing := MissingChunks(session)
		sendJSONResponse(w, ChunkStatusResponse{
			Success:       true,
			UploadID:      session.UploadID,
			ReceivedCount: len(session.ReceivedChunks) - len(missing),
			TotalChunks:   session.TotalChunks,
			MissingChunks: missing,
			Completed:     session.Completed,
			FileChecksum:  session.FileChecksum,
			Filename:      session.Filename,
			Message:       fmt.Sprintf("会话%d/%d分块已接收", len(session.ReceivedChunks)-len(missing), session.TotalChunks),
		}, http.StatusOK)
		return true
	}
	// upload_id 存在但 session 不存在
	if filename == "" {
		sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return true
	}
	return false
}

// lookupFilenameStatus 按 filename 查找上传会话或检查文件是否已存在。返回 true 表示已处理请求。
func (h *Handlers) lookupFilenameStatus(w http.ResponseWriter, owner, filename string) bool {
	// 防御性校验：防止路径穿越
	if _, err := ValidateFilePath(filename); err != nil {
		sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return true
	}
	store := h.uploadStoreFor(owner)
	if store == nil {
		return h.checkFileExistsStatus(w, owner, filename)
	}
	session := store.GetSessionByFilename(filename)
	if session != nil {
		missing := MissingChunks(session)
		sendJSONResponse(w, ChunkStatusResponse{
			Success:       true,
			UploadID:      session.UploadID,
			ReceivedCount: len(session.ReceivedChunks) - len(missing),
			TotalChunks:   session.TotalChunks,
			MissingChunks: missing,
			Completed:     session.Completed,
			FileChecksum:  session.FileChecksum,
			Filename:      session.Filename,
		}, http.StatusOK)
		return true
	}

	return h.checkFileExistsStatus(w, owner, filename)
}

// checkFileExistsStatus 检查租户 user 桶内文件是否已存在且 checksum 匹配。
// 返回 true 表示已处理请求。
func (h *Handlers) checkFileExistsStatus(w http.ResponseWriter, owner, filename string) bool {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return true
	}
	rel, ok := tnt.UserRel(filename)
	if !ok {
		sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return true
	}
	root := tnt.Root()
	stat, err := root.Stat(rel)
	if err != nil {
		return false
	}
	if cs := h.checksumStoreFor(owner); cs != nil {
		if checksum, ok := cs.Get(rel); ok {
			sendJSONResponse(w, ChunkStatusResponse{
				Success:      true,
				Completed:    true,
				FileChecksum: checksum,
				Filename:     filename,
				Message:      fmt.Sprintf(errFmtFileExists, stat.Size()),
			}, http.StatusOK)
			return true
		}
	}
	// 有文件但无 checksum 记录（意外情况），实时计算
	if cs, err := FileChecksumRoot(root, rel); err == nil {
		sendJSONResponse(w, ChunkStatusResponse{
			Success:      true,
			Completed:    true,
			FileChecksum: cs,
			Filename:     filename,
			Message:      fmt.Sprintf(errFmtFileExists, stat.Size()),
		}, http.StatusOK)
		return true
	}
	return false
}

// validateCompleteSession 校验 complete 请求的 session 是否有效。
// 如果校验失败，已发送错误响应，返回 (nil, false)。
func (h *Handlers) validateCompleteSession(w http.ResponseWriter, store *UploadStore, owner, uploadID string) (*ChunkedUploadSession, bool) {
	if uploadID == "" {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "缺少 upload_id"}, http.StatusBadRequest)
		return nil, false
	}
	// 租户隔离靠 per-tenant store：跨租户同裸 id 会话在此 store 中不存在 → 404
	session := store.GetSession(uploadID)
	if session == nil {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return nil, false
	}

	h.logger.Info("uploadComplete 开始", "upload_id", uploadID, "file_name", session.Filename,
		"received", countReceived(session.ReceivedChunks), "total", session.TotalChunks)

	if session.Completed {
		h.logger.Info("上传已完成（幂等）", "upload_id", uploadID, "file_name", session.Filename)
		sendJSONResponse(w, ChunkCompleteResponse{
			Success:      true,
			Filename:     session.Filename,
			FileChecksum: session.FileChecksum,
			Message:      "上传已完成",
		}, http.StatusOK)
		return nil, false
	}

	if !store.AllChunksReceived(uploadID) {
		session = store.GetSession(uploadID)
		missing := MissingChunks(session)
		h.logger.Warn("合并请求时还有分块未接收", "upload_id", uploadID, "missing", len(missing))
		sendJSONResponse(w, ChunkCompleteResponse{
			Success: false,
			Message: fmt.Sprintf("还有 %d 个分块未接收", len(missing)),
		}, http.StatusBadRequest)
		return nil, false
	}

	return session, true
}

// mergeAndRenameFile 合并分块到租户 user 桶内的临时文件，校验 SHA-256，然后原子重命名。
// 目标路径经 Tenant.UserRel 映射到 user 桶（与 upload/download 读写一致，防符号链接逃逸）。
// 如果操作失败，已发送错误响应，返回 ("", false)。
func (h *Handlers) mergeAndRenameFile(ctx context.Context, w http.ResponseWriter, store *UploadStore, owner, uploadID string, session *ChunkedUploadSession) (string, bool) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", false
	}
	rel, ok := tnt.UserRel(session.Filename)
	if !ok {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", false
	}
	root := tnt.Root()

	// 确保目标文件的父目录存在（user 桶内相对路径）
	dir := filepath.Dir(rel)
	if err := root.MkdirAll(dir, 0755); err != nil {
		h.logger.Error("创建目标父目录失败", "upload_id", uploadID, "file_name", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "创建目标目录失败"}, http.StatusInternalServerError)
		return "", false
	}

	// 在目标同目录创建唯一临时文件（root 相对，O_EXCL 防碰撞），合并完成后 root.Rename 原子替换
	tmpRel := filepath.Join(dir, filepath.Base(rel)+".tmp."+fmt.Sprintf("%d", time.Now().UnixNano()))
	tmpFile, err := root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		h.logger.Error("创建合并临时文件失败", "upload_id", uploadID, "file_name", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
		return "", false
	}
	defer tmpFile.Close()
	defer func() { _ = root.Remove(tmpRel) }()

	finalChecksum, err := h.mergeChunksWithHash(ctx, store, uploadID, session, tmpFile)
	if err != nil {
		return "", false
	}

	if err := tmpFile.Close(); err != nil {
		h.logger.Error("关闭合并文件失败", "upload_id", uploadID, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "关闭目标文件失败"}, http.StatusInternalServerError)
		return "", false
	}

	// 校验最终文件的 SHA-256
	if finalChecksum != session.FileChecksum {
		h.logger.Error("最终文件 SHA-256 校验失败", "server", finalChecksum, "client", session.FileChecksum)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "最终文件 SHA-256 校验失败，文件未保存"}, http.StatusBadRequest)
		return "", false
	}

	// 原子重命名为最终文件名（租户根内）
	if err := atomicRenameRoot(root, tmpRel, rel); err != nil {
		h.logger.Error("重命名最终文件失败", "upload_id", uploadID, "file_name", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return "", false
	}

	return finalChecksum, true
}

// recordCompleteMetadata 记录文件 checksum、保留时间戳并清理上传 session。
func (h *Handlers) recordCompleteMetadata(owner, uploadID string, session *ChunkedUploadSession, finalChecksum string) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		h.logger.Warn("记录完成元数据失败：租户不可用", "owner", owner)
		return
	}
	rel, ok := tnt.UserRel(session.Filename)
	if !ok {
		h.logger.Warn("记录完成元数据失败：文件名映射失败", "owner", owner, "file_name", session.Filename)
		return
	}
	root := tnt.Root()

	// 保留文件原始修改时间
	if session.FileModTime > 0 {
		modTime := time.Unix(0, session.FileModTime)
		if err := root.Chtimes(rel, modTime, modTime); err != nil {
			h.logger.Warn("设置文件时间戳失败", "file_name", session.Filename, "error", err)
		}
	}

	// 记录 checksum（per-tenant store，key = 租户根内相对路径 rel）
	if cs := h.checksumStoreFor(owner); cs != nil {
		cs.Set(rel, finalChecksum)
	} else {
		h.logger.Warn("per-tenant checksum store 不可用，跳过记录", "owner", owner)
	}

	// 标记完成（延迟清理 session 目录）
	store := h.uploadStoreFor(owner)
	if store == nil {
		h.logger.Warn("per-tenant UploadStore 不可用，跳过 session 清理", "owner", owner)
		return
	}
	if err := store.CompleteSession(uploadID); err != nil {
		h.logger.Warn("标记 session 完成失败", "upload_id", uploadID, "error", err)
	}
	// 异步清理 session 目录
	store.CleanupSessionAfter(uploadID, 5*time.Second)
}

// uploadComplete 合并所有分块完成上传。
func (h *Handlers) uploadComplete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, size.CompleteBodyLimit)
	var req struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	owner := ownerFromRequest(r)
	store := h.uploadStoreFor(owner)
	if store == nil {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: errMsgUploadIDNotFound}, http.StatusNotFound)
		return
	}
	session, ok := h.validateCompleteSession(w, store, owner, req.UploadID)
	if !ok {
		return
	}

	// 合并不随客户端断开而取消：Using WithoutCancel 派生独立 context，使
	// sclient CLI 上传大文件后断开连接/超时不影响已在进行的合并；
	// 即使响应已写出，合并也能完成（下次 init/status/complete 幂等发现已上传）。
	// （mergeChunksWithHash 内部仍保守检查 ctx.Done：本 context 永不 cancel，
	// recovery 兜底走进程级；未来如需可打断用独立 goroutine + 状态表。）
	mergeCtx := context.WithoutCancel(r.Context())

	finalChecksum, ok := h.mergeAndRenameFile(mergeCtx, w, store, owner, req.UploadID, session)
	if !ok {
		// mergeAndRenameFile 已发送错误响应；分块与会话保留，客户端可重试 init/complete。
		h.logger.Warn("合并失败但分块保留，客户端可重试", "upload_id", req.UploadID, "file_name", session.Filename)
		return
	}

	// P4 配额对账：预留 → 实际落地（合并后的 user 文件 = TotalSize）。
	// session 为 GetSession 副本，Reservation 指针与存储会话共享，Commit 原子生效一次；
	// 后续 CleanupSessionAfter → DeleteSession 的 Release 为空操作。
	if session.Reservation != nil {
		session.Reservation.Commit(session.TotalSize)
		session.Reservation = nil
	}

	h.recordCompleteMetadata(owner, req.UploadID, session, finalChecksum)

	h.logger.Info("文件合并完成", "file_name", session.Filename, "checksum", shortid.ShortHash(finalChecksum), "size", session.TotalSize)
	sendJSONResponse(w, ChunkCompleteResponse{
		Success:      true,
		Filename:     session.Filename,
		FileChecksum: finalChecksum,
		Message:      "文件合并并校验通过",
	}, http.StatusOK)
}

// mergeChunksWithHash 读取所有分块顺序写入 outFile，同时计算 SHA-256 并返回 hex 摘要。
// 在循环中检查 ctx.Done() 以支持取消，避免大文件合并时 OOM。
func (h *Handlers) mergeChunksWithHash(ctx context.Context, store *UploadStore, uploadID string, session *ChunkedUploadSession, outFile *os.File) (string, error) {
	hasher := sha256.New()
	multiWriter := io.MultiWriter(outFile, hasher)

	for i := 0; i < session.TotalChunks; i++ {
		select {
		case <-ctx.Done():
			h.logger.Warn("合并被取消", "upload_id", uploadID, "received", i, "total", session.TotalChunks, "error", ctx.Err())
			return "", ctx.Err()
		default:
		}
		if err := h.mergeOneChunk(ctx, store, uploadID, i, multiWriter); err != nil {
			h.logger.Error("合并 chunk 失败", "upload_id", uploadID, "chunk_index", i, "error", err)
			return "", err
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// mergeOneChunk 读取单个 chunk 文件并把内容拷贝到 dst。
// 获取 chunk 合并写锁：等待所有正在写入的 chunk 完成后才允许读取，
// 阻塞新的 chunk 写入，避免读到不完整的 chunk。
func (h *Handlers) mergeOneChunk(ctx context.Context, store *UploadStore, uploadID string, idx int, dst io.Writer) error {
	chunkPath := store.ChunkFilePath(uploadID, idx)
	// 获取 chunk 合并写锁：等待所有正在写入的 chunk 完成后才允许读取，
	// 阻塞新的 chunk 写入，避免读到不完整的 chunk。
	unlockMerge := store.LockChunkMerge(uploadID)
	defer unlockMerge()
	chunkFile, err := os.Open(chunkPath)
	if err != nil {
		return fmt.Errorf("打开 chunk %d 失败: %w", idx, err)
	}
	defer chunkFile.Close()
	// 使用 io.Copy 写入目标，同时通过 ctx.Done() 支持取消
	if _, err := io.Copy(dst, chunkFile); err != nil {
		return fmt.Errorf("拷贝 chunk %d 失败: %w", idx, err)
	}
	return nil
}
