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
	"strings"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/quota"
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
	// 文件存在但 checksum 不匹配：versioning 开启时视为有意覆盖旧版本（进入分块流程，
	// 由 complete 先 saveVersion 备份再覆盖，配额完整对账）；否则不允许覆盖。
	if cfg := h.cfgPtr.Load(); cfg != nil && cfg.Versioning.Enabled {
		h.logger.Info("同名文件已存在但 checksum 不匹配，versioning 开启视为覆盖",
			"file_name", filename, "old_size", stat.Size())
		return false
	}
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

	// 已存在同名文件的检查（租户 user 桶）——命中（already_exists / checksum 冲突）时
	// 不建临时名、不预留，直接返回。
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

	// 任务 4 设计决策②：同名存活 session 同 checksum 复用续传、不同 checksum 直拒（Conflict）。
	// 预检：存在未完成同名会话但 checksum/大小不一致 → 拒绝（避免同目标两个在途会话）。
	if existing := store.GetSessionByFilename(req.Filename); existing != nil {
		if existing.FileChecksum != req.FileChecksum || existing.TotalSize != req.TotalSize {
			h.logger.Warn("同名会话已存在但 checksum 不匹配，拒绝创建新会话",
				"file_name", req.Filename, "upload_id", req.UploadID)
			sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "同名文件正在上传中且 checksum 不一致"}, http.StatusConflict)
			return
		}
	}

	// 会话直接以裸 id 创建于本租户 store（无 owner 前缀；隔离靠 per-tenant chunk 桶）
	session, reused, err := store.GetOrCreateSession(req.UploadID, req.Filename,
		req.TotalSize, chunkSize, req.TotalChunks, req.FileChecksum, req.FileModTime)
	if err != nil {
		h.logger.Error("创建/续传上传会话失败", "upload_id", req.UploadID, "error", err)
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
		return
	}

	if !reused {
		// 任务 4：在途整文件（user 桶目标同目录）与配额预留。
		// 先 TryReserve(TotalSize) 于 user 桶 Scope（507 时清理 session 返回 507，
		// 不创建临时名），再 O_EXCL 建临时名 + Truncate(TotalSize) 防跨 worker 冲突。
		// 临时名过 storage.ValidSegmentName，不以 .inflight 开头的 .part 会被扫描按普通
		// 文件计入 user 桶配额（此处已 TryReserve，账本一致）；会话记录 tempPath。
		// 全局兜底由 Scope 父链自动生效；未装配 quota 时回退旧 storageMgr 预留。
		scope := h.quotaBucketFor(owner, "user")
		if scope != nil {
			rr, reserveErr := scope.TryReserve(session.TotalSize)
			if reserveErr != nil {
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
			// P5 回退预留登记：会话删除/过期/完成时按此释放（DeleteSession/cleanupExpired）。
			session.StorageMgrReserved = session.TotalSize
		}

		// 创建在途整临时文件（user 桶 target 同目录，O_EXCL 防跨 worker 冲突），
		// Truncate(TotalSize) 预先占位；失败按 500，临时名未创建无需清理配额。
		// tempRel = user/<dir>/.inflight-<hash16>-<upload_id>.part（散列取 rel 全路径）。
		tempRel := tempRelForUser(session, rel)
		if tempRel == "" {
			h.logger.Error("派生在途临时文件路径失败", "upload_id", session.UploadID, "file_name", session.Filename)
			store.DeleteSession(session.UploadID)
			sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		// 确保临时名父目录存在（user/<dir> 桶目标同目录）。
		if err := tnt.Root().MkdirAll(filepath.Dir(tempRel), 0o755); err != nil {
			h.logger.Error("创建在途临时文件父目录失败", "upload_id", session.UploadID, "error", err)
			store.DeleteSession(session.UploadID)
			sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		tmpFile, err := tnt.Root().OpenFile(tempRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			h.logger.Error("创建在途临时文件失败", "upload_id", session.UploadID, "error", err)
			store.DeleteSession(session.UploadID)
			sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		if err := tmpFile.Truncate(session.TotalSize); err != nil {
			tmpFile.Close()
			_ = tnt.Root().Remove(tempRel)
			h.logger.Error("预占在途临时文件失败", "upload_id", session.UploadID, "error", err)
			store.DeleteSession(session.UploadID)
			sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		if err := tmpFile.Close(); err != nil {
			_ = tnt.Root().Remove(tempRel)
			h.logger.Error("关闭在途临时文件失败", "upload_id", session.UploadID, "error", err)
			store.DeleteSession(session.UploadID)
			sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
			return
		}
		session.TempPath = tempRel
		// 回写 session.json 持久化 tempPath（重启后据此恢复续传）。
		if err := store.PersistNow(session.UploadID); err != nil {
			h.logger.Warn("持久化在途临时文件路径失败", "upload_id", session.UploadID, "error", err)
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

// tempRelForUser 生成租户 user 桶内分块在途整文件的存储根相对路径：
// user/<dir>/.inflight-<sha256(rel)前16hex>-<upload_id>.part，与正式名同目录。
// rel 为 tnt.UserRel(filename) 结果（user/... 相对路径）；inflightTempName 对 rel 取散列
// 并拼 uploadID 段，返回段名安全（散列 + uploadID 均无非法字符）。dir 由 filepath.Dir
// 导出（rel 内路径段已由 UserRel 校验合法）；返回空串表示非法 rel。
func tempRelForUser(session *ChunkedUploadSession, rel string) string {
	userRel := strings.TrimPrefix(rel, "user/")
	dir := filepath.Dir(filepath.FromSlash(userRel))
	if dir == "." {
		dir = ""
	}
	tempName := inflightTempName(rel, session.UploadID)
	if dir == "" {
		return "user/" + tempName
	}
	return "user/" + filepath.ToSlash(dir) + "/" + tempName
}

// chunkLenAt 返回会话中第 i 个分片的实际长度（末片可能短于 chunk_size）。
func chunkLenAt(session *ChunkedUploadSession, i int) int64 {
	offset := int64(i) * session.ChunkSize
	if offset >= session.TotalSize {
		return 0
	}
	if remaining := session.TotalSize - offset; remaining < session.ChunkSize {
		return remaining
	}
	return session.ChunkSize
}

// openSessionTemp 打开会话的在途整临时文件（绝对路径，root.OpenFile 相对保证不逃逸）。
// 只读（供 complete 全文件读取与恢复校验）；写入由 chunk 写路径单独以 O_WRONLY 打开。
func (h *Handlers) openSessionTemp(tnt *storage.Tenant, session *ChunkedUploadSession) (*os.File, error) {
	abs, ok := tnt.Root().Abs(session.TempPath)
	if !ok {
		return nil, fmt.Errorf("在途临时文件路径越界: %s", session.TempPath)
	}
	return os.Open(abs)
}

// openSessionTempWrite 打开会话在途整临时文件用于 seek 直写（O_WRONLY）。
func (h *Handlers) openSessionTempWrite(tnt *storage.Tenant, session *ChunkedUploadSession) (*os.File, error) {
	abs, ok := tnt.Root().Abs(session.TempPath)
	if !ok {
		return nil, fmt.Errorf("在途临时文件路径越界: %s", session.TempPath)
	}
	return os.OpenFile(abs, os.O_WRONLY, 0)
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

	// 获取 chunk IO 读锁（任务 4：并发分段写各自 seek 固定 offset + BoundWriter 防越界，
	// 锁域仍按 uploadID 划分避免同会话 bitmap 更新与完成读的竞态；complete 用写锁读全文件）。
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

	// 任务 4：seek+BoundWriter 直写整临时文件。不写独立 .chunk 文件。
	// 流程：读块到内存 → 块 checksum 校验 → 清空读取句柄（读指针已 EOF）→
	// Seek(i*chunkSize) → BoundWriter(limit=该分片实际长度) 写入（防越界写坏相邻分片）→
	// MarkChunkReceived(i, checksum)。请求体已受 MaxBytesReader(DefaultChunkBodyLimit)
	// 限制，单块 ≤ ~60 MiB（测试 4KiB），内存缓冲可控。
	// 乱序安全：seek 固定 offset + BoundWriter 逐段写，互不覆盖；并发分段写沿用锁。
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "上传会话缺少在途临时文件"}, http.StatusInternalServerError)
		return
	}
	if session.TempPath == "" {
		// 任务 4：会话缺临时名（旧磁盘遗留/篡改）。本分片无法直写——拒绝并提示
		// 客户端重试 init（重新创建临时名），不静默吞掉分片。
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "上传会话缺少在途临时文件，请重新初始化"}, http.StatusInternalServerError)
		return
	}

	// 读请求块到内存并计算 SHA-256（一次性，双用：校验 + 直写数据源）。
	data, err := io.ReadAll(file)
	if err != nil {
		h.logger.Error("读取分块失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "读取分块失败"}, http.StatusInternalServerError)
		return
	}
	if closeErr := file.Close(); closeErr != nil {
		h.logger.Error("关闭分块读取句柄失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", closeErr)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "读取分块失败"}, http.StatusInternalServerError)
		return
	}
	serverChecksum := fmt.Sprintf("%x", sha256.Sum256(data))
	if serverChecksum != chunkChecksum {
		h.logger.Warn("chunk SHA-256 不匹配", "upload_id", uploadID, "chunk_index", chunkIndex,
			"server", shortid.ShortHash(serverChecksum), "client", shortid.ShortHash(chunkChecksum),
			"session_chunk_size", session.ChunkSize)
		sendJSONResponse(w, ChunkUploadResponse{
			Success:     false,
			ChunkIndex:  chunkIndex,
			ShouldRetry: true,
			Message:     "SHA-256 校验不匹配",
		}, http.StatusOK)
		return
	}

	// 限长分片直写：limit=该分片实际长度（末片短于 chunk_size）。
	offset := int64(chunkIndex) * session.ChunkSize
	limit := chunkLenAt(session, chunkIndex)
	written, err := h.writeChunkDirect(session, tnt, offset, limit, data)
	if err != nil {
		h.logger.Error("写入在途临时文件失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "写入分块失败"}, http.StatusInternalServerError)
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

// writeChunkDirect 把已通过 checksum 校验的分片数据 seek 直写进在途整临时文件。
// root 相对 TempPath → Abs 派生绝对路径（防符号链接逃逸）+ os.OpenFile O_WRONLY；
// NewBoundWriter(offset, limit) 在 [offset, offset+limit) 限长写入（超限 io.EOF，防越界
// 写坏相邻分片），offset 保证乱序直写互不覆盖。返回实际写入字节数；data 超长时截断，
// 不足 limit 时按实际写（末片短于 chunk_size 属正常）。
func (h *Handlers) writeChunkDirect(session *ChunkedUploadSession, tnt *storage.Tenant, offset, limit int64, data []byte) (int64, error) {
	tmpFile, err := h.openSessionTempWrite(tnt, session)
	if err != nil {
		return 0, err
	}
	defer tmpFile.Close()

	bw := quota.NewBoundWriter(tmpFile, offset, limit, 0)
	n, err := bw.Write(data)
	if err != nil && err != io.EOF {
		return int64(n), fmt.Errorf("限长写入分片失败: %w", err)
	}
	return int64(n), nil
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
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "请求体必须是合法 JSON（无法解析）"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "请求体校验失败（签名哈希不匹配或 JSON 语法错误）"}, http.StatusBadRequest)
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

	// 合并不随客户端断开而取消：Using WithoutCancel 派生独立 context。
	// （complete 内部仍保守检查 ctx.Done；recovery 兜底走进程级。）
	mergeCtx := context.WithoutCancel(r.Context())

	// 取 owner 的租户与 user 桶相对路径（覆盖写 ReleaseUsage / complete 用）。
	tnt := h.tenantFor(owner)
	rel := ""
	if tnt != nil && tnt.Root() != nil {
		if r, ok := tnt.UserRel(session.Filename); ok {
			rel = r
		}
	}

	// 全文件校验临时名内容 == session.FileChecksum：
	//  校验通过 → rename 为正式名 → 写 checksum store → 覆盖写 ReleaseUsage(old)；
	//  校验失败 → 逐分片 seek 重算 → mismatch_chunks（失败保留 session+临时名+预留供重传，
	//   不释放——重传还要写临时名；只有取消/过期/放弃才释放，见 DeleteSession/cleanupExpired）。
	mismatch, err := h.prepareMergedTemp(mergeCtx, store, tnt, session)
	if err != nil {
		// 全文件校验失败且已定位坏分片 → 400 + mismatch_chunks；IO/内部错误 → 500。
		if mismatch != nil {
			h.logger.Warn("complete 校验失败，客户端按 mismatch_chunks 重传坏分片",
				"upload_id", req.UploadID, "file_name", session.Filename, "mismatch", mismatch)
			sendJSONResponse(w, ChunkCompleteResponse{
				Success:        false,
				Filename:       session.Filename,
				Message:        fmt.Sprintf("%d 个分片校验失败，请重传这些分片后再次完成", len(mismatch)),
				MismatchChunks: mismatch,
			}, http.StatusBadRequest)
			return
		}
		h.logger.Error("合并分块失败", "upload_id", req.UploadID, "file_name", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "合并文件失败"}, http.StatusInternalServerError)
		return
	}

	// 覆盖写（versioning enabled + 目标存同名旧文件）先备份版本。saveVersion 把旧文件
	// 复制进 version 桶（version 桶 Scope 记账），不改 user 桶 committed；失败 best-effort。
	// 任务 8 O-1：覆盖动作记审计（沿用 upload_handler 覆盖写审计写法，Action=overwrite）。
	overwrote := false
	if rel != "" && tnt != nil && tnt.Root() != nil {
		if cfg := h.cfgPtr.Load(); cfg != nil && cfg.Versioning.Enabled {
			if _, sErr := tnt.Root().Stat(rel); sErr == nil {
				if _, vErr := h.saveVersion(strings.TrimPrefix(rel, "user/"), tnt, owner); vErr != nil {
					h.logger.Warn("保存文件版本失败", "file_name", session.Filename, "error", vErr)
				} else {
					overwrote = true
				}
			}
		}
	}

	// rename 前 stat 旧文件大小（覆盖写）；新文件场景 old=0。
	prev := int64(0)
	if rel != "" && tnt != nil && tnt.Root() != nil {
		if st, statErr := tnt.Root().Stat(rel); statErr == nil {
			prev = st.Size()
		}
	}
	finalChecksum := session.FileChecksum
	if err := atomicRenameRoot(tnt.Root(), session.TempPath, rel); err != nil {
		h.logger.Error("重命名最终文件失败", "upload_id", req.UploadID, "file_name", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}

	// P4/P5 配额对账（I1）：user 桶 Scope——init 已 TryReserve(TotalSize) 预留新文件全部
	// 字节（容量已在 init 校验），此处把预留 Commit 成 user 桶 committed（新文件大小），
	// 覆盖写再 ReleaseUsage(old) 释放已无磁盘实体的旧文件字节。净效果：committed 恰好等于
	// 新文件真实大小（显式对账，替代 Adjust 差分）。Release 原子生效一次——CompleteSession
	// 后 CleanupSessionAfter 删除会话时的额外 Release/Commit 为空操作。
	if scope := h.quotaBucketFor(owner, "user"); scope != nil {
		if session.Reservation != nil {
			// 先提交新文件字节（reserved → committed），再释放旧文件字节。
			session.Reservation.Commit(session.TotalSize)
			session.Reservation = nil
		}
		if prev > 0 {
			// 覆盖写：rename 已原子替换，旧文件字节从磁盘消失 → ReleaseUsage(old)。
			scope.ReleaseUsage(prev)
		}
	}

	// 写 checksum store（per-tenant key = rel，与 download 读取一致）。
	if cs := h.checksumStoreFor(owner); cs != nil {
		cs.Set(rel, finalChecksum)
	} else {
		h.logger.Warn("per-tenant checksum store 不可用，跳过记录", "owner", owner)
	}

	// 任务 8 O-1：分块上传覆盖写（rename 已原子替换旧文件）记审计，与 upload_handler
	// 覆盖写审计写法一致；无覆盖（新文件）不审计（普通上传成功也不记 audit，保持一致）。
	if overwrote {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "overwrite", ObjectType: "file", Object: session.Filename,
			Result: AuditResultSuccess, Detail: "分块上传覆盖现有文件（版本已保存）",
		})
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

// prepareMergedTemp 在 complete 期对临时名做全文件校验（== file_checksum）并逐分片准确
// 报告 mismatch。校验通过返回 (nil, nil)；全文件校验失败返回 (mismatchList, err)；
// 临时文件被外部删除/不可读返回 (mismatchList, err)（findMismatchChunks 对缺失返回全部分片
// mismatch → 调用方按 400 返回 mismatch_chunks，客户端整文件重传，而非 500 永久挂起）。
// 做法：持 LockChunkMerge 排他（防 chunk 并发 seek 写）后单遍哈希整临时文件比对
// file_checksum —— 不匹配再逐分片 seek 重算（带长度语义 offset=i*ChunkSize、length=
// chunkLenAt，与写侧/恢复侧一致）→ 精确定位坏分片 → ClearChunksReceived 落盘 bitmap
// （status 亦反映需重传列表）。不复用上传期的独立 .chunk 文件（任务 4 起不存在）。
func (h *Handlers) prepareMergedTemp(ctx context.Context, store *UploadStore, tnt *storage.Tenant, session *ChunkedUploadSession) ([]int, error) {
	if tnt == nil || tnt.Root() == nil || session.TempPath == "" {
		return nil, fmt.Errorf("会话缺少在途临时文件，无法完成上传")
	}
	unlockMerge := store.LockChunkMerge(session.UploadID)
	defer unlockMerge()

	src, err := h.openSessionTemp(tnt, session)
	if err != nil {
		// 任务 8 M-3：临时文件缺失/不可读 → findMismatchChunks 返回全部分片 index（客户端
		// 整文件重传），而非 500（临时名命中 isInflightTempName 不入列表，此处按 mismatch 显式化）。
		if os.IsNotExist(err) {
			return allMismatchIndices(session), err
		}
		return nil, fmt.Errorf("打开在途临时文件失败: %w", err)
	}
	defer src.Close()

	// 单遍整文件哈希。ctx 由 WithoutCancel 派生，永不 cancel；保守检查保留。
	hf := sha256.New()
	if _, err := io.Copy(hf, src); err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		return nil, fmt.Errorf("读取在途临时文件失败: %w", err)
	}
	if hex.EncodeToString(hf.Sum(nil)) == session.FileChecksum {
		return nil, nil // 全文件校验通过
	}

	// 全文件校验失败：逐分片 seek 重算 mismatch（I-2：重叠/越界写坏单片被精确定位）。
	mismatch := store.findMismatchChunks(session)
	if len(mismatch) == 0 {
		// 理论不可达（整文件哈希不同但每个分片哈希都匹配），防御：全部视为 mismatch。
		mismatch = allMismatchIndices(session)
	}
	// 落盘 bitmap：坏分片清位（重复 complete 仍返回同样的 mismatch；status 反映需重传）。
	if err := store.ClearChunksReceived(session.UploadID, mismatch); err != nil {
		h.logger.Error("complete mismatch 清位失败", "upload_id", session.UploadID, "error", err)
	}
	return mismatch, fmt.Errorf("分块校验失败：%d 个分片不匹配", len(mismatch))
}
