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

// mergeAndRenameFile 把会话在途临时文件经全文件校验后原子重命名为正式名（user 桶）。
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

	finalChecksum, err := h.mergeChunksWithHash(ctx, store, uploadID, session, tnt, tmpFile)
	if err != nil {
		h.logger.Error("合并失败", "upload_id", uploadID, "file_name", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "合并文件失败"}, http.StatusInternalServerError)
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

	// 原子重命名为最终文件名（租户根内）。在途临时文件名仍在磁盘上（同 user 桶目录）——
	// 由 complete 的配额对账随后释放预留 + DeleteSession 清理临时名。
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

	// 配额记账：合并前先统计目标 user 文件当前大小 prev（覆盖写场景用 Adjust；正常 chunked 流程
	// init 已拒绝覆盖，prev=0）。必须在合并前 stat——合并后文件已落盘，stat 恒为新文件大小，
	// 会把"新增"误判为"覆盖写"导致 Adjust 差分 0、user 桶从不记账（I1 修复的关键）。
	tnt := h.tenantFor(owner)
	var rel string
	prev := int64(0)
	if tnt != nil && tnt.Root() != nil {
		if r, relOK := tnt.UserRel(session.Filename); relOK {
			rel = r
			if st, statErr := tnt.Root().Stat(rel); statErr == nil {
				prev = st.Size()
			}
		}
	}

	finalChecksum, ok := h.mergeAndRenameFile(mergeCtx, w, store, owner, req.UploadID, session)
	if !ok {
		// mergeAndRenameFile 已发送错误响应；分块与会话保留，客户端可重试 init/complete。
		h.logger.Warn("合并失败但分块保留，客户端可重试", "upload_id", req.UploadID, "file_name", session.Filename)
		return
	}

	// P4/P5 配额对账（I1 修复）：合并后的最终文件落 **user 桶**，chunk 会话目录即将清理。
	// 1) 先归还 chunk 桶预留（chunk 字节不再归属 chunk 桶）；若继续 Commit 到 chunk 桶会造成
	//    user 桶从未记账、chunk 桶永久虚高、delete 释放钳 0——桶级错位泄漏。
	// 2) 再到 **user 桶** Scope 上记账：新文件 TryReserve+Commit(actual)；覆盖写（罕见）Adjust(prev, actual)。
	// session 为 GetSession 副本，Reservation 指针与存储会话共享，Release 原子生效一次。
	if session.Reservation != nil {
		session.Reservation.Release()
		session.Reservation = nil
	}
	if scope := h.quotaBucketFor(owner, "user"); scope != nil {
		actual := session.TotalSize
		removeMerged := func() {
			if tnt != nil && tnt.Root() != nil && rel != "" {
				_ = tnt.Root().Remove(rel)
			}
		}
		if prev > 0 {
			// 覆盖写（正常 chunked 流程 init 已拒绝覆盖，此处防御）：容量预检后 Adjust 差分。
			if actual > prev {
				extra, reserveErr := scope.TryReserve(actual - prev)
				if reserveErr != nil {
					// 覆盖写竞态 + 配额不足：合并已用新内容替换旧文件，removeMerged 删除后
					// 磁盘无文件，user 桶仍记着旧文件 prev 字节——同步 ReleaseUsage(prev) 使
					// 账本与磁盘一致（否则旧文件字节虚高直至周期扫描校准）。
					removeMerged()
					scope.ReleaseUsage(prev)
					sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
					return
				}
				scope.Adjust(prev, actual)
				extra.Release()
			} else {
				scope.Adjust(prev, actual)
			}
		} else {
			rr, reserveErr := scope.TryReserve(actual)
			if reserveErr != nil {
				removeMerged()
				sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
				return
			}
			rr.Commit(actual)
		}
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

// mergeChunksWithHash 把会话的在途整临时文件全量拷贝到 outFile，同时计算 SHA-256
// 返回 hex 摘要。任务 4 最小可用 complete：临时文件 seek 直写完成后已含全文件内容，
// 这里做整文件校验 + rename（mismatch_chunks 精确化与覆盖写精确化属任务 5）。
// tnt 为 session 所属租户（在途临时文件在其 user 桶）。临时文件不存在/校验失败返回
// error（调用方保留分块与会话供重试）。
func (h *Handlers) mergeChunksWithHash(ctx context.Context, store *UploadStore, uploadID string, session *ChunkedUploadSession, tnt *storage.Tenant, outFile *os.File) (string, error) {
	// 获取 chunk 合并写锁：等待所有正在写入的分片完成后才允许读取整文件，
	// 阻塞新的分片写入，避免读到不完整的临时文件。
	unlockMerge := store.LockChunkMerge(uploadID)
	defer unlockMerge()

	if tnt == nil || tnt.Root() == nil || session.TempPath == "" {
		return "", fmt.Errorf("会话缺少在途临时文件，无法完成上传")
	}
	src, err := h.openSessionTemp(tnt, session)
	if err != nil {
		return "", fmt.Errorf("打开在途临时文件失败: %w", err)
	}
	defer src.Close()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outFile, hasher)
	if _, err := io.Copy(multiWriter, src); err != nil {
		// 保守检查 ctx.Done（本 context 由 WithoutCancel 派生，永不 cancel；保留检查）。
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		return "", fmt.Errorf("拷贝在途临时文件失败: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
