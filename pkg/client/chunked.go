// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
)

const (
	// defaultConcurrency 是**上传**默认并发（受网盘写配额限制，保守取 4）。
	defaultConcurrency = 4
	// defaultDownloadConcurrency 是**下载**默认并发（2026-10-24 实测：热路径 4→16
	// 并发使 2.1GB 下载 104.9s → 54.7s；取 8 作为默认，兼顾网盘读配额与磁盘吞吐）。
	defaultDownloadConcurrency = 8
	maxRetries                 = 3

	// UploadIDAlreadyExists 是服务端返回的特殊 upload_id 值，表示文件已存在（通过 checksum 匹配）。
	// 此值由服务端协议定义，变更需同步更新。
	UploadIDAlreadyExists = "already_exists"

	// DownloadKindCloudArchive 与服务端 download kind 保持一致（cloud_archive）。
	// 归档文件存服务端租户 archive 桶（内部桶），普通下载不开放内部桶路径访问，
	// kind 方案由服务端在归档桶内按 owner 拼接。
	DownloadKindCloudArchive = "cloud_archive"
	// DownloadKindCloudTask 与服务端 download kind 保持一致（cloud_task）。
	// 云任务原始文件存服务端租户 cloud 桶（内部桶），经 kind 由服务端校验任务
	// owner 后拼接下载（RemotePath 传 <taskID>/<file>）。
	DownloadKindCloudTask = "cloud_task"
)

// ChunkedUploadResult 表示分块上传的结果。
type ChunkedUploadResult struct {
	Success      bool   `json:"success"`
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename,omitempty"`
	FileChecksum string `json:"file_checksum,omitempty"`
	TotalChunks  int    `json:"total_chunks,omitempty"`
	Message      string `json:"message,omitempty"`
	// MismatchChunks 是服务端 complete 全文件校验失败后逐分片 seek 重算出的坏分片索引
	// （协议字段 mismatch_chunks，客户端据此只重传这些分片而不清空整个会话）。服务器端
	// 任务 5 起返回；旧服务端无此字段（保持省略）。
	MismatchChunks []int `json:"mismatch_chunks,omitempty"`
}

// chunkedInitRequest 分块上传初始化请求体。
type chunkedInitRequest struct {
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename"`
	TotalSize    int64  `json:"total_size"`
	ChunkSize    int64  `json:"chunk_size"`
	TotalChunks  int    `json:"total_chunks"`
	FileChecksum string `json:"file_checksum"`
	FileModTime  int64  `json:"file_mod_time"` // UnixNano
	Volume       string `json:"volume,omitempty"`
	// MetaExtra 首次初始化即携带的自定义元数据（body 可装，避免 header 24KiB 截断风险）。
	MetaExtra map[string]any `json:"meta_extra,omitempty"`
}

// chunkedCompleteRequest 分块上传完成请求体。
type chunkedCompleteRequest struct {
	UploadID string `json:"upload_id"`
}

// uploadCacheEntry 缓存文件 checksum，用于避免重复计算。
// createdAt 用于 TTL 过期淘汰，防止长时间运行后缓存无限增长。
type uploadCacheEntry struct {
	fileSize     int64
	modTime      time.Time
	fileChecksum string
	createdAt    time.Time
}

const (
	defaultMaxCacheEntries = 1000             // 默认缓存最大条目数，超过时触发过期清理
	defaultCacheTTL        = 10 * time.Minute // 默认缓存条目 TTL
)

// resumeSessionParams 是 tryResumeSession 和 initNewUploadSession 的参数结构体，
// 用于减少函数参数数量（S107）。
type resumeSessionParams struct {
	UploadID     string
	Filename     string
	LocalPath    string
	FileChecksum string
	FileSize     int64
	ChunkSize    int64
	TotalChunks  int
	Concurrency  int
	ModTime      time.Time
	MetaExtra    map[string]any // 首次 init 携带（续传会话已有则忽略）
}

// downloadChunkParams 是 downloadOneChunk 的参数结构体，用于减少函数参数数量（S107）。
type downloadChunkParams struct {
	Filename  string
	Kind      string // 下载 kind（空=普通文件；cloud_archive=归档）
	ChunkIdx  int
	ChunkSize int64
	FileSize  int64
	OutFile   *os.File
	Mu        *sync.Mutex
	Progress  *int64
	Cancel    context.CancelFunc
	Done      <-chan struct{}
	// Failed 是**失败计数共享桶**（2026-10-23）：单个 chunk 重试耗尽后记录到
	// 桶内（而非直接 p.Cancel 拖垮整个下载）——主流程收尾时对失败 chunk 单独重试，
	// 仍失败才 fail-closed 删除输出文件。外部卷大文件（4MiB chunk + 网盘慢）下，
	// 偶发超时不应让一次下载静默产出零填充损坏文件。
	Failed *sync.Map
}

// tryResumeResult 是 tryResumeSession 的返回值类型，用于替代三返回值模式。
type tryResumeResult struct {
	result         *ChunkedUploadResult
	serverUploadID string // 服务端返回的完整 session id（可能带 owner 前缀）；空表示沿用 p.UploadID
	shouldContinue bool
	err            error
}

// calcChunkSize 根据文件大小自适应计算分块大小。
// preferred 为首选分块大小（默认 4 MiB），maxChunk 为最大限制（默认 64 MiB）。
func calcChunkSize(fileSize, preferred, maxChunk int64) int64 {
	if maxChunk <= 0 {
		maxChunk = size.DefaultMaxChunkSize
	}
	if preferred <= 0 {
		preferred = size.DefaultChunkSize
	}
	chunkSize := min(preferred, maxChunk)
	if fileSize > 0 {
		for chunkSize < maxChunk {
			if chunkSize > math.MaxInt64/512 {
				chunkSize = maxChunk
				break
			}
			if chunkSize*512 >= fileSize {
				break
			}
			chunkSize *= 2
			if chunkSize > maxChunk {
				chunkSize = maxChunk
				break
			}
		}
	}
	return chunkSize
}

// generateUploadID 根据文件元数据生成确定性的 upload_id。
func generateUploadID(filename string, fileSize int64, modTime time.Time, fileChecksum string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%d|%d|%s", filename, fileSize, modTime.UnixNano(), fileChecksum)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ChunkedUploader 封装分块上传的并发控制和进度追踪。
type ChunkedUploader struct {
	client      *FileClient
	chunkSize   int64
	concurrency int
	fileSize    int64
	totalChunks int
	filePath    string
	filename    string
	uploadID    string // 当前会话 upload_id（re-init 后会被替换；经 uploadIDMu 保护读写）
	checksum    string
	failed      atomic.Bool
	mu          sync.Mutex
	progress    int64

	// 自动重新 init（CHUNK C-2 客户端自愈）所需字段：
	//   - baseInitID 是**原始确定性 upload_id**（generateUploadID 结果，re-init 时基于它派生新 id）；
	//   - fileModTime 是 init 请求需要的文件修改时间；
	//   - reinitOnce 保证整个上传最多触发一次自动 re-init；
	//   - needFullReupload 置位后 run() 会基于**新会话**全量重传所有分块
	//     （旧会话已成功的分块在新会话中不存在，必须重传）；
	//   - uploadIDMu 保护 uploadID 的并发读写（多个 chunk goroutine + complete 并发访问）。
	baseInitID       string
	fileModTime      time.Time
	reinitOnce       sync.Once
	needFullReupload atomic.Bool
	uploadIDMu       sync.RWMutex
}

// chunkedUploaderOpts 是 newChunkedUploader 的参数集合，用于减少函数参数数量（go:S107）。
type chunkedUploaderOpts struct {
	client      *FileClient
	filePath    string
	uploadID    string
	chunkSize   int64
	fileSize    int64
	totalChunks int
	checksum    string
	filename    string
	concurrency int
	modTime     time.Time
}

// newChunkedUploader 创建分块上传器。
func newChunkedUploader(opts chunkedUploaderOpts) *ChunkedUploader {
	return &ChunkedUploader{
		client:      opts.client,
		chunkSize:   opts.chunkSize,
		concurrency: opts.concurrency,
		fileSize:    opts.fileSize,
		totalChunks: opts.totalChunks,
		filePath:    opts.filePath,
		filename:    opts.filename,
		uploadID:    opts.uploadID,
		checksum:    opts.checksum,
		baseInitID:  opts.uploadID,
		fileModTime: opts.modTime,
	}
}

// getUploadID 返回当前会话 upload_id（uploadIDMu 保护；re-init 后返回新会话 id）。
func (u *ChunkedUploader) getUploadID() string {
	u.uploadIDMu.RLock()
	defer u.uploadIDMu.RUnlock()
	return u.uploadID
}

// setUploadID 更新当前会话 upload_id（仅 re-init 路径调用）。
func (u *ChunkedUploader) setUploadID(id string) {
	u.uploadIDMu.Lock()
	u.uploadID = id
	u.uploadIDMu.Unlock()
}

// completeMaxAttempts 是 complete 遇 mismatch_chunks 后的最大重试次数。
// 每次重试只重传上次报告的分片；若服务端持续报告新 mismatch（临时文件被并发破坏等），
// 达到上限后返回错误（含最后一次报告的 mismatch 分片，调用方决策）。
const completeMaxAttempts = 3

// run 执行分块上传循环：先上传指定索引列表的分块，再 complete。若 complete 返回
// mismatch_chunks，仅重传这些分片后再次 complete（有界重试）。
func (u *ChunkedUploader) run(ctx context.Context, chunkIndices []int) (*ChunkedUploadResult, error) {
	if u.concurrency <= 0 {
		u.concurrency = 1
	}
	u.uploadChunkIndices(ctx, chunkIndices)
	u.reuploadIfNeeded(ctx)
	lastResult, err := u.completeWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	u.client.logger.InfoContext(ctx, "分块上传完成", "file_name", u.filename,
		"checksum", shortid.ShortHash(u.checksum))
	return lastResult, nil
}

// reuploadIfNeeded 会话缺少在途临时文件触发自动 re-init 后，新会话 bitmap 全空 ⇒
// 必须全量重传（旧会话已成功的分块在新会话中不存在）。needFullReupload 由
// handleTempMissing 置位。
func (u *ChunkedUploader) reuploadIfNeeded(ctx context.Context) {
	if !u.needFullReupload.Load() || u.failed.Load() {
		return
	}
	u.client.logger.WarnContext(ctx, "重新初始化后全量重传所有分块",
		"file_name", u.filename, "total_chunks", u.totalChunks)
	all := make([]int, u.totalChunks)
	for i := range all {
		all[i] = i
	}
	u.uploadChunkIndices(ctx, all)
}

// completeWithRetry 先 complete，遇 mismatch_chunks 仅重传这些分片后再次 complete；
// 有界重试（completeMaxAttempts 次）。返回成功结果或终态错误。
func (u *ChunkedUploader) completeWithRetry(ctx context.Context) (*ChunkedUploadResult, error) {
	var lastResult *ChunkedUploadResult
	for range completeMaxAttempts {
		if u.failed.Load() {
			return nil, fmt.Errorf("上传失败：部分分块上传失败，可使用 --resume 续传")
		}
		completeResult, err := u.completeOnce(ctx)
		if err != nil {
			return nil, err
		}
		lastResult = completeResult
		if completeResult.Success {
			break
		}
		if len(completeResult.MismatchChunks) == 0 {
			// 非 mismatch 类失败（会话过期/校验失败等）→ 直接失败，不盲目重传。
			return nil, fmt.Errorf("文件合并失败: %s", completeResult.Message)
		}
		// mismatch：只重传服务端报告的分片（seek 覆盖），再 complete。
		u.client.logger.WarnContext(ctx, "complete 返回 mismatch，重传坏分片",
			"upload_id", shortid.ShortHash(u.getUploadID()), "mismatch", completeResult.MismatchChunks)
		u.uploadChunkIndices(ctx, completeResult.MismatchChunks)
	}
	if lastResult == nil || !lastResult.Success {
		if lastResult != nil && len(lastResult.MismatchChunks) > 0 {
			return nil, fmt.Errorf("分块上传未完成：仍有 %d 个分片校验失败（已达重试上限）: %s",
				len(lastResult.MismatchChunks), lastResult.Message)
		}
		return nil, fmt.Errorf("文件合并失败")
	}
	return lastResult, nil
}

// uploadChunkIndices 把指定分片索引列表经并发池上传（失败经 uploadChunkWithRetry 退避重试）。
// failed 置位后跳过剩余分片。
func (u *ChunkedUploader) uploadChunkIndices(ctx context.Context, chunkIndices []int) {
	if len(chunkIndices) == 0 {
		return
	}
	sem := make(chan struct{}, u.concurrency)
	var wg sync.WaitGroup

	taskCh := make(chan int, len(chunkIndices))
	for _, idx := range chunkIndices {
		taskCh <- idx
	}
	close(taskCh)

	for idx := range taskCh {
		if u.failed.Load() {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(chunkIdx int) {
			defer wg.Done()
			defer func() { <-sem }()
			u.uploadChunkWithRetry(ctx, chunkIdx)
		}(idx)
	}
	wg.Wait()
}

// completeOnce 发送一次 /upload/complete 并解析响应（含 mismatch_chunks）。
//
// I-1（任务 6）：协议层统一「先读响应体再判状态」——无论状态码是否 2xx，都先读响应体
// 解析 JSON；解析出 MismatchChunks 即返回结果（哪怕 success=false），绝不因非 JSON body
// 或传输层把非 2xx 提早判错而丢失 MismatchChunks。非 JSON body（旧服务端/代理 500 纯文本）
// 才返回携带 body 文本的确定性错误，调用方可判错决策。
func (u *ChunkedUploader) completeOnce(ctx context.Context) (*ChunkedUploadResult, error) {
	completeBody, _ := json.Marshal(chunkedCompleteRequest{UploadID: u.getUploadID()})
	// **complete 跳过客户端级超时（2026-10-23 实测）**：服务端 complete 触发 staging→
	// 网盘转存（大文件 8-10 分钟），默认 300s 客户端超时会让请求先断而服务端仍在写。
	// 走正常 doRequest 装配（签名/认证/追踪全保留），仅由 ctx 标记跳过 Client.Timeout。
	resp, err := u.client.doRequest(WithNoClientTimeout(ctx), "POST", "/upload/complete",
		bytes.NewReader(completeBody), http.Header{
			headerContentType: {"application/json"},
		})
	if err != nil {
		return nil, fmt.Errorf("完成上传请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 先读限长 body（防恶意超大响应），再尝试 JSON 解析。若解析成功——即便 status 非 2xx
	// 或 success=false——也返回结果，让调用方（run）基于 Success/MismatchChunks/Message
	// 决策，而不是在此判定错误（否则 mismatch 信息丢失）。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var completeResult ChunkedUploadResult
	if json.Unmarshal(raw, &completeResult) == nil {
		// 解析成功：返回结果（MismatchChunks/Message 均保留）。
		return &completeResult, nil
	}
	// 非 JSON body：确定性错误，Message 携带原始 body（截断），供调用方判错。
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 512 {
		msg = msg[:512] + "..."
	}
	if msg == "" {
		msg = "空响应体"
	}
	return &completeResult, fmt.Errorf("解析 complete 响应失败（HTTP %d）: %s", resp.StatusCode, msg)
}

// uploadChunkWithRetry 在 goroutine 中执行分块上传，包含重试逻辑。
// 成功时直接返回；失败且需要重试时继续重试循环；非重试错误或重试耗尽时标记 failed。
func (u *ChunkedUploader) uploadChunkWithRetry(ctx context.Context, chunkIdx int) {
	for range maxRetries {
		if u.failed.Load() {
			return
		}

		if u.uploadChunk(ctx, chunkIdx) {
			return
		}
		if u.failed.Load() {
			return
		}
	}
	u.client.logger.WarnContext(ctx, "chunk 重试耗尽", "chunk_index", chunkIdx,
		"upload_id", shortid.ShortHash(u.getUploadID()))
	u.failed.Store(true)
}

// uploadChunk 执行一个分块的完整上传流程（打开文件、读取、构建请求、发送、解析响应）。
// 返回 true 表示上传成功，false 表示需要重试（对于不可重试的错误，内部调用 u.failed.Store(true)）。
func (u *ChunkedUploader) uploadChunk(ctx context.Context, chunkIdx int) bool {
	f, err := u.openAndSeekChunk(chunkIdx)
	if err != nil {
		u.client.logger.WarnContext(ctx, "chunk 打开文件失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.getUploadID()), "file", u.filePath, "error", err)
		return false
	}

	offset := int64(chunkIdx) * int64(u.chunkSize)
	chunkData := make([]byte, u.chunkSize)
	n, readErr := io.ReadFull(f, chunkData)
	f.Close()
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		u.client.logger.WarnContext(ctx, "chunk 读取失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.getUploadID()), "offset", offset, "error", readErr)
		return false
	}
	chunkData = chunkData[:n]

	// 计算分块 SHA-256
	chunkHash := sha256.Sum256(chunkData)
	chunkChecksum := hex.EncodeToString(chunkHash[:])

	// 构造 multipart 请求
	body, ct, err := u.buildChunkRequest(ctx, chunkIdx, chunkData, chunkChecksum)
	if err != nil {
		u.client.logger.WarnContext(ctx, "chunk 构建请求失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.getUploadID()), "error", err)
		return false
	}

	success, shouldRetry, statusCode, message := u.sendChunkRequest(ctx, chunkIdx, body, ct)
	if success {
		u.mu.Lock()
		u.progress += int64(n)
		progress := u.progress
		u.mu.Unlock()

		if u.client.progressFn != nil {
			u.client.progressFn("上传", progress, u.fileSize)
		}
		u.client.logger.DebugContext(ctx, "chunk 上传成功", "chunk_index", chunkIdx, "checksum", shortid.ShortHash(chunkChecksum))
		return true
	}

	if !shouldRetry {
		// 非重试错误（如 upload_id 过期），标记失败
		u.client.logger.WarnContext(ctx, "chunk 非重试错误", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.getUploadID()), "status", statusCode,
			"message", message)
		u.failed.Store(true)
		return false
	}

	// 会话缺少在途临时文件（CHUNK C-2）：服务端返回 500 + should_retry + 该文案，
	// 意味着当前会话无法修复（旧磁盘遗留/篡改 ⇒ TempPath 为空）。客户端自愈：
	// 自动重新 init（新 upload_id 派生自原始确定性 id）并标记全量重传。
	// 返回 false 且不置 failed，让 uploadChunkWithRetry 的重试循环用新会话继续。
	if strings.Contains(message, "缺少在途临时文件") {
		u.handleTempMissing(ctx)
	}
	return false
}

// handleTempMissing 处理「会话缺少在途临时文件」：自动重新 init 一个**新会话**。
//
// 为什么必须用新 upload_id（而非原始确定性 id）：服务端 init 按 id 复用未完成会话
// （GetOrCreateSession，元数据一致即复用）⇒ 原始 id 会复用同一个坏会话 ⇒ 死循环。
// 派生新 id（原确定性 id + "-reinit"）⇒ 服务端建新会话（新 TempPath）⇒ 可修复。
// 旧坏会话在服务端占用的预留由 TTL 清理（罕见场景，可接受）。
//
// 整个上传最多触发一次（reinitOnce）；re-init 失败 ⇒ 标记 failed（重试循环随后退出）。
func (u *ChunkedUploader) handleTempMissing(ctx context.Context) {
	u.reinitOnce.Do(func() {
		u.client.logger.WarnContext(ctx, "chunk 会话缺少在途临时文件，自动重新初始化",
			"file_name", u.filename, "old_upload_id", shortid.ShortHash(u.getUploadID()))

		newID := u.baseInitID + "-reinit"
		serverUploadID, newChunkSize, newTotalChunks, err := u.client.initNewUploadSession(ctx, resumeSessionParams{
			UploadID:     newID,
			Filename:     u.filename,
			FileChecksum: u.checksum,
			FileSize:     u.fileSize,
			ChunkSize:    u.chunkSize,
			TotalChunks:  u.totalChunks,
			ModTime:      u.fileModTime,
		})
		if err != nil {
			u.client.logger.ErrorContext(ctx, "自动重新初始化失败", "file_name", u.filename, "error", err)
			u.failed.Store(true)
			return
		}
		if newTotalChunks == -1 {
			// 文件已存在（幂等命中）：等价成功。
			u.client.logger.InfoContext(ctx, "重新初始化时发现文件已存在", "file_name", u.filename)
			u.failed.Store(false)
			return
		}
		u.setUploadID(serverUploadID)
		if newChunkSize != u.chunkSize {
			u.chunkSize = newChunkSize
			u.totalChunks = newTotalChunks
		}
		// 新会话 bitmap 全空 ⇒ 必须全量重传（旧会话已成功的分块在新会话中不存在）。
		u.needFullReupload.Store(true)
		u.client.logger.WarnContext(ctx, "已重新初始化新会话，将全量重传",
			"file_name", u.filename, "new_upload_id", shortid.ShortHash(serverUploadID),
			"total_chunks", u.totalChunks)
	})
}

// openAndSeekChunk 打开文件并寻道到指定分块的偏移位置。
func (u *ChunkedUploader) openAndSeekChunk(index int) (*os.File, error) {
	offset := int64(index) * int64(u.chunkSize)
	f, err := os.Open(u.filePath)
	if err != nil {
		return nil, err
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// buildChunkRequest 构建分块上传的 multipart 请求体，返回 body reader 和 Content-Type。
// 使用 io.Pipe 流式构建，避免 bytes.Buffer 完整副本，降低大文件上传时的内存峰值。
// 对于 4 并发 × 64 MiB 的极端场景，此优化可消除 ~260 MiB 的额外内存分配。
func (u *ChunkedUploader) buildChunkRequest(ctx context.Context, chunkIdx int, chunkData []byte, chunkChecksum string) (io.Reader, string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mw.Close()
		streamChunkMultipart(ctx, pw, mw, chunkIdx, chunkData, chunkChecksum, u.getUploadID)
	}()

	return pr, mw.FormDataContentType(), nil
}

// streamChunkMultipart 在 io.Pipe 写侧构建分块 multipart 体（upload_id/chunk_index/
// chunk_checksum 字段 + form file 数据段）。任一阶段 ctx 取消或写入失败即关闭 pw。
func streamChunkMultipart(ctx context.Context, pw *io.PipeWriter, mw *multipart.Writer, chunkIdx int, chunkData []byte, chunkChecksum string, getUploadID func() string) {
	if chunkPipeAborted(ctx, pw) {
		return
	}
	fields := []struct{ k, v string }{
		{"upload_id", getUploadID()},
		{"chunk_index", strconv.Itoa(chunkIdx)},
		{"chunk_checksum", chunkChecksum},
	}
	for _, f := range fields {
		if !writeChunkField(ctx, pw, mw, f.k, f.v) {
			return
		}
	}
	if chunkPipeAborted(ctx, pw) {
		return
	}
	writeChunkFormFile(ctx, pw, mw, chunkIdx, chunkData)
}

// chunkPipeAborted 检查 ctx 是否已取消，是则关闭 pw 并返回 true。
func chunkPipeAborted(ctx context.Context, pw *io.PipeWriter) bool {
	select {
	case <-ctx.Done():
		pw.CloseWithError(ctx.Err())
		return true
	default:
		return false
	}
}

// writeChunkField 写入一个 multipart 文本字段；失败时关闭 pw 返回 false。
func writeChunkField(ctx context.Context, pw *io.PipeWriter, mw *multipart.Writer, field, value string) bool {
	if chunkPipeAborted(ctx, pw) {
		return false
	}
	if err := mw.WriteField(field, value); err != nil {
		pw.CloseWithError(fmt.Errorf("写入 %s: %w", field, err))
		return false
	}
	return true
}

// writeChunkFormFile 写入分块数据段（异步写 + ctx 取消时可中断）。
func writeChunkFormFile(ctx context.Context, pw *io.PipeWriter, mw *multipart.Writer, chunkIdx int, chunkData []byte) {
	if chunkPipeAborted(ctx, pw) {
		return
	}
	part, err := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", chunkIdx))
	if err != nil {
		pw.CloseWithError(fmt.Errorf("创建 form file: %w", err))
		return
	}
	errCh := make(chan error, 1)
	go func() {
		_, werr := part.Write(chunkData)
		errCh <- werr
	}()
	select {
	case <-ctx.Done():
		pw.CloseWithError(ctx.Err())
	case werr := <-errCh:
		if werr != nil {
			pw.CloseWithError(fmt.Errorf("写入 form part: %w", werr))
		}
	}
}

// sendChunkRequest 发送分块上传请求并解析响应，返回 success、shouldRetry、statusCode、message。
func (u *ChunkedUploader) sendChunkRequest(ctx context.Context, chunkIdx int, body io.Reader, contentType string) (success, shouldRetry bool, statusCode int, message string) {
	headers := make(http.Header)
	headers.Set(headerContentType, contentType)

	chunkResp, err := u.client.doRequest(ctx, "POST", "/upload/chunk", body, headers)
	if err != nil {
		u.client.logger.WarnContext(ctx, "chunk 上传请求失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.getUploadID()), "error", err)
		// doRequest 失败时关闭 body reader（io.Pipe），避免 buildChunkRequest 的
		// goroutine 在 pipe 写入时阻塞泄漏
		if closer, ok := body.(io.Closer); ok {
			closer.Close()
		}
		return false, true, 0, ""
	}
	defer chunkResp.Body.Close()

	var chunkResult struct {
		Success     bool   `json:"success"`
		ShouldRetry bool   `json:"should_retry"`
		Message     string `json:"message"`
	}
	if decodeErr := json.NewDecoder(io.LimitReader(chunkResp.Body, 1<<20)).Decode(&chunkResult); decodeErr != nil {
		u.client.logger.WarnContext(ctx, "chunk 响应解析失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.getUploadID()), "status", chunkResp.StatusCode,
			"error", decodeErr)
		return false, true, chunkResp.StatusCode, ""
	}

	return chunkResult.Success, chunkResult.ShouldRetry, chunkResp.StatusCode, chunkResult.Message
}

// calcFileChecksum 计算文件的 SHA-256 checksum，同时处理缓存。
// 与 calculateChecksum（无缓存，位于 client.go）不同，此函数使用 TTL 过期清理的缓存机制。
// 返回校验和、是否从缓存获取、错误。
//
// 缓存策略：使用 TTL 过期 + 每次 Store 触发清理的主动淘汰机制。
//   - 缓存命中时检查 mtime+size 和 TTL（懒清理），过期则重新计算
//   - 写入缓存时 Range 清理过期条目
//
// 选择在 Store 时 Range 清理而非后台 goroutine，避免 goroutine 生命周期管理
// 注意：sync.Map 无内置 Len()，无法精确控制条目数上限。
// 当前方案通过 TTL 过期 + 每次 Store 触发清理来防止缓存无限增长。
// 对于 maxCacheEntries=1000 的默认值，即使有少量偏差，内存开销也可控。
//   - 选择此方案而非纯懒清理：纯懒清理在 SDK 长时间运行场景下，不命中的条目
//     永不清除，导致内存泄漏。主动淘汰确保内存上限可预测。
//   - 选择此方案而非后台 goroutine 定期清理：避免 goroutine 生命周期管理，
//     且 Range 清理只在写入时触发，对正常上传路径无额外开销。
func (c *FileClient) calcFileChecksum(localPath string, file *os.File, fileSize int64, modTime time.Time) (string, bool, error) {
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return "", false, fmt.Errorf("计算绝对路径失败: %w", err)
	}
	if cached, ok := c.uploadCache.Load(absPath); ok {
		entry, ok := cached.(*uploadCacheEntry)
		switch {
		case !ok:
			c.uploadCache.Delete(absPath)
		case time.Since(entry.createdAt) > c.cacheTTL:
			c.uploadCache.Delete(absPath)
			c.logger.Debug("checksum 缓存过期", "file_path", localPath)
		case entry.fileSize == fileSize && entry.modTime.Equal(modTime):
			c.logger.Debug("checksum 缓存命中", "file_path", localPath)
			return entry.fileChecksum, true, nil
		}
	}

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", false, fmt.Errorf("计算 SHA-256 失败: %w", err)
	}
	fileChecksum := hex.EncodeToString(h.Sum(nil))
	// 主动淘汰：使用 atomic.Int64 计数器，每 Store 10 次触发一次 Range 清理，
	// 避免每次 Store 都 O(n) 遍历全表。
	// 注意：并发写入时 Range 可能遗漏极少数条目，但足以触发清理
	const cacheCleanInterval = 10
	if c.cacheCleanCounter.Add(1)%cacheCleanInterval == 0 {
		var entryCount int
		c.uploadCache.Range(func(k, v any) bool {
			entry := v.(*uploadCacheEntry) //nolint:errcheck
			if time.Since(entry.createdAt) > c.cacheTTL {
				c.uploadCache.Delete(k)
				return true
			}
			entryCount++
			if c.maxCacheEntries > 0 && entryCount > c.maxCacheEntries {
				c.uploadCache.Delete(k)
			}
			return true
		})
	}

	c.uploadCache.Store(absPath, &uploadCacheEntry{
		fileSize:     fileSize,
		modTime:      modTime,
		fileChecksum: fileChecksum,
		createdAt:    time.Now(),
	})
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", false, fmt.Errorf("重置文件指针失败: %w", err)
	}
	c.logger.Debug("文件 SHA-256 计算完毕", "file_path", localPath, "checksum", shortid.ShortHash(fileChecksum))
	return fileChecksum, false, nil
}

// tryResumeSession 尝试续传已有的上传会话。
// 返回专用结果类型 tryResumeResult。
func (c *FileClient) tryResumeSession(ctx context.Context, p resumeSessionParams) tryResumeResult {
	statusResp, statusErr := c.doRequest(ctx, "GET",
		fmt.Sprintf("/upload/status?upload_id=%s&filename=%s", p.UploadID, url.QueryEscape(p.Filename)), nil, nil)
	if statusErr != nil || statusResp.StatusCode != http.StatusOK {
		if statusResp != nil {
			statusResp.Body.Close()
		}
		return tryResumeResult{shouldContinue: true}
	}

	var statusData struct {
		Success       bool   `json:"success"`
		Finished      bool   `json:"finished"`
		UploadID      string `json:"upload_id"`
		ReceivedCount int    `json:"received_count"`
		TotalChunks   int    `json:"total_chunks"`
		MissingChunks []int  `json:"missing_chunks"`
		Completed     bool   `json:"completed"`
		FileChecksum  string `json:"file_checksum"`
		Message       string `json:"message"`
	}
	if json.NewDecoder(io.LimitReader(statusResp.Body, 1<<20)).Decode(&statusData) != nil || !statusData.Success {
		statusResp.Body.Close()
		return tryResumeResult{shouldContinue: true}
	}
	statusResp.Body.Close()

	if statusData.Finished || statusData.Completed {
		c.logger.InfoContext(ctx, "文件已存在，直接返回成功", "file_name", p.Filename, "checksum", shortid.ShortHash(p.FileChecksum))
		return tryResumeResult{result: &ChunkedUploadResult{
			Success:      true,
			UploadID:     p.UploadID,
			Filename:     p.Filename,
			FileChecksum: p.FileChecksum,
			Message:      "文件已存在",
		}, shouldContinue: false}
	}

	if statusData.UploadID != "" {
		// 续传命中：采用服务端返回的 session id（任务 12 起为裸 id，无 owner 前缀；
		// 租户隔离由服务端 per-tenant chunk 桶物理保证）。后续 chunk/complete 沿用该 id。
		serverID := statusData.UploadID
		c.logger.InfoContext(ctx, "续传会话已恢复", "upload_id", shortid.ShortHash(serverID),
			"missing", len(statusData.MissingChunks), "total", statusData.TotalChunks)
		result, err := c.uploadChunks(ctx, statusData.MissingChunks, chunkUploadOpts{
			filePath:     p.LocalPath,
			uploadID:     serverID,
			chunkSize:    p.ChunkSize,
			fileSize:     p.FileSize,
			totalChunks:  statusData.TotalChunks,
			fileChecksum: p.FileChecksum,
			filename:     p.Filename,
			concurrency:  p.Concurrency,
			modTime:      p.ModTime,
		})
		return tryResumeResult{result: result, serverUploadID: serverID, err: err, shouldContinue: false}
	}

	return tryResumeResult{shouldContinue: true}
}

// initNewUploadSession 创建新的上传 session，并返回（服务端 session 完整 id、服务端 chunk_size、总块数）。
// 服务端在多租户下会对会话 key 做 owner 前缀隔离，返回的 upload_id 可能带前缀，
// 后续 chunk/complete/status 必须沿用该完整 id（否则 404）。
func (c *FileClient) initNewUploadSession(ctx context.Context, p resumeSessionParams) (string, int64, int, error) {
	initBody := chunkedInitRequest{
		UploadID:     p.UploadID,
		Filename:     p.Filename,
		TotalSize:    p.FileSize,
		ChunkSize:    p.ChunkSize,
		TotalChunks:  p.TotalChunks,
		FileChecksum: p.FileChecksum,
		FileModTime:  p.ModTime.UnixNano(),
		Volume:       c.volume,
		MetaExtra:    p.MetaExtra,
	}
	initJSON, _ := json.Marshal(initBody)

	resp, err := c.doRequest(ctx, "POST", "/upload/init", bytes.NewReader(initJSON), http.Header{
		headerContentType: {"application/json"},
	})
	if err != nil {
		return "", 0, 0, fmt.Errorf("初始化上传失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", 0, 0, fmt.Errorf("初始化上传失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var initResult struct {
		Success   bool   `json:"success"`
		UploadID  string `json:"upload_id"`
		ChunkSize int64  `json:"chunk_size"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&initResult); err != nil {
		return "", 0, 0, fmt.Errorf("解析 init 响应失败: %w", err)
	}
	if !initResult.Success {
		return "", 0, 0, fmt.Errorf("初始化上传失败: %s", initResult.Message)
	}

	// 如果 upload_id = "already_exists"，说明文件已存在且 checksum 匹配
	if initResult.UploadID == UploadIDAlreadyExists {
		return "", 0, -1, nil // -1 表示已存在
	}

	// 服务端可能返回带 owner 前缀的 session id（多租户隔离），后续请求必须沿用
	serverUploadID := initResult.UploadID
	if serverUploadID == "" {
		serverUploadID = p.UploadID
	}

	newChunkSize := p.ChunkSize
	if initResult.ChunkSize > 0 {
		newChunkSize = initResult.ChunkSize
	}
	newTotalChunks := int((p.FileSize + newChunkSize - 1) / newChunkSize)
	return serverUploadID, newChunkSize, newTotalChunks, nil
}

// ChunkedUpload 分块上传文件到指定的远端路径。支持续传。
//
// 参数：
//   - ctx: 上下文
//   - localPath: 本地文件路径
//   - remotePath: 远端路径（如 "dir1/file.txt"）
//   - opts: 可选参数
//
// 可选参数通过 ChunkedOption 函数设置：
//   - WithChunkedChunkSize(size): 分块大小
//   - WithChunkedConcurrency(n): 并发数
//   - WithChunkedResume(enabled): 续传模式
func (c *FileClient) ChunkedUpload(ctx context.Context, localPath, remotePath string, opts ...ChunkedOption) (*ChunkedUploadResult, error) {
	// 解析选项
	opt := &chunkedOpts{
		chunkSize:   c.chunkSize,
		concurrency: defaultConcurrency,
		resume:      true,
	}
	for _, o := range opts {
		o(opt)
	}
	maxChunk := c.maxChunkSize
	if maxChunk <= 0 {
		maxChunk = size.DefaultMaxChunkSize
	}

	file, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %w", err)
	}
	fileSize := stat.Size()
	modTime := stat.ModTime()

	if fileSize <= 0 {
		return nil, fmt.Errorf("chunked upload: 不支持空文件或空文件")
	}

	var fileChecksum string
	fileChecksum, _, err = c.calcFileChecksum(localPath, file, fileSize, modTime)
	if err != nil {
		return nil, err
	}

	// 自适应分块大小
	chunkSize := calcChunkSize(fileSize, opt.chunkSize, maxChunk)
	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)
	filename := filepath.ToSlash(filepath.Clean(remotePath))
	uploadID := generateUploadID(filename, fileSize, modTime, fileChecksum)

	c.logger.InfoContext(ctx, "分块上传开始", "file_name", filename, "file_size", fileSize,
		"chunk_size", chunkSize, "total_chunks", totalChunks, "upload_id", shortid.ShortHash(uploadID))

	// 尝试续传
	if opt.resume {
		res := c.tryResumeSession(ctx, resumeSessionParams{
			UploadID:     uploadID,
			Filename:     filename,
			LocalPath:    localPath,
			FileChecksum: fileChecksum,
			FileSize:     fileSize,
			ChunkSize:    chunkSize,
			TotalChunks:  totalChunks,
			Concurrency:  opt.concurrency,
			MetaExtra:    opt.metaExtra,
		})
		// 续传命中时 tryResumeSession 内部已用服务端完整 session id 上传缺失分块并 complete，
		// 返回的 result 就是最终结果（续传路径无需再改 uploadID）。
		if !res.shouldContinue {
			return res.result, res.err
		}
	}

	// 新文件 / 不在上传中，创建新 session
	c.logger.InfoContext(ctx, "新上传", "file_name", filename, "upload_id", shortid.ShortHash(uploadID))

	serverUploadID, newChunkSize, newTotalChunks, err := c.initNewUploadSession(ctx, resumeSessionParams{
		UploadID:     uploadID,
		Filename:     filename,
		FileChecksum: fileChecksum,
		FileSize:     fileSize,
		ChunkSize:    chunkSize,
		MetaExtra:    opt.metaExtra,
		TotalChunks:  totalChunks,
		ModTime:      modTime,
	})
	if err != nil {
		return nil, err
	}
	if newTotalChunks == -1 {
		c.logger.InfoContext(ctx, "文件已存在，直接返回成功", "file_name", filename)
		return &ChunkedUploadResult{
			Success:      true,
			UploadID:     UploadIDAlreadyExists,
			Filename:     filename,
			FileChecksum: fileChecksum,
		}, nil
	}
	// 服务端在多租户下会返回带 owner 前缀的 session id，后续 chunk/complete/status
	// 必须沿用该完整 id（否则 404）。同时自适应采用服务端协商的 chunk_size。
	if serverUploadID != uploadID {
		c.logger.InfoContext(ctx, "服务端返回带 owner 前缀的 session id", "old", shortid.ShortHash(uploadID), "new", shortid.ShortHash(serverUploadID))
		uploadID = serverUploadID
	}
	if newChunkSize != chunkSize {
		chunkSize = newChunkSize
		totalChunks = newTotalChunks
		c.logger.InfoContext(ctx, "服务端返回的 chunk_size", "chunk_size", chunkSize, "total_chunks", totalChunks)
	}

	// 上传全部分块
	allChunks := make([]int, totalChunks)
	for i := 0; i < totalChunks; i++ {
		allChunks[i] = i
	}
	return c.uploadChunks(ctx, allChunks, chunkUploadOpts{
		filePath:     localPath,
		uploadID:     uploadID,
		chunkSize:    chunkSize,
		fileSize:     fileSize,
		totalChunks:  totalChunks,
		fileChecksum: fileChecksum,
		filename:     filename,
		concurrency:  opt.concurrency,
		modTime:      modTime,
	})
}

// chunkUploadOpts 是 uploadChunks 的参数集合，用于减少函数参数数量（go:S107）。
type chunkUploadOpts struct {
	filePath     string
	uploadID     string
	chunkSize    int64
	fileSize     int64
	totalChunks  int
	fileChecksum string
	filename     string
	concurrency  int
	modTime      time.Time
}

// uploadChunks 上传指定索引列表的分块，然后完成上传。
func (c *FileClient) uploadChunks(ctx context.Context, chunkIndices []int, opts chunkUploadOpts) (*ChunkedUploadResult, error) {
	uploader := newChunkedUploader(chunkedUploaderOpts{
		client:      c,
		filePath:    opts.filePath,
		uploadID:    opts.uploadID,
		chunkSize:   opts.chunkSize,
		fileSize:    opts.fileSize,
		totalChunks: opts.totalChunks,
		checksum:    opts.fileChecksum,
		filename:    opts.filename,
		concurrency: opts.concurrency,
		modTime:     opts.modTime,
	})
	return uploader.run(ctx, chunkIndices)
}

// downloadParams 分块下载的参数。
type downloadParams struct {
	chunkSize   int64
	concurrency int
	maxChunk    int64
	kind        string
}

// getDownloadParams 解析分块下载的选项参数。
func getDownloadParams(c *FileClient, opts ...ChunkedOption) *downloadParams {
	opt := &chunkedOpts{
		chunkSize:   c.chunkSize,
		concurrency: defaultDownloadConcurrency,
	}
	for _, o := range opts {
		o(opt)
	}
	maxChunk := c.maxChunkSize
	if maxChunk <= 0 {
		maxChunk = size.DefaultMaxChunkSize
	}
	return &downloadParams{
		chunkSize:   opt.chunkSize,
		concurrency: opt.concurrency,
		maxChunk:    maxChunk,
		kind:        opt.kind,
	}
}

// getFileStat 通过 HEAD 请求获取远端文件的元信息。
// kind 非空时追加 &kind=<kind>（如 cloud_archive），服务端据此解析归档目录。
func getFileStat(ctx context.Context, c *FileClient, filename, kind string) (fileSize int64, checksum string, modTime int64, err error) {
	statPath := "/api/files/stat?filename=" + url.QueryEscape(filename)
	if kind != "" {
		statPath += "&kind=" + url.QueryEscape(kind)
	} else {
		statPath += c.volumeQueryPart()
	}
	statResp, err := c.doRequest(ctx, "HEAD", statPath, nil, nil)
	if err == nil && statResp.StatusCode == http.StatusOK {
		if s := statResp.Header.Get("X-File-Size"); s != "" {
			fileSize, _ = strconv.ParseInt(s, 10, 64)
		}
		checksum = statResp.Header.Get(headerFileChecksum)
		if m := statResp.Header.Get(headerFileMTime); m != "" {
			modTime, _ = strconv.ParseInt(m, 10, 64)
		}
	}
	if statResp != nil {
		statResp.Body.Close()
	}

	if fileSize <= 0 {
		return 0, "", 0, fmt.Errorf("无法获取文件信息: %s", filename)
	}
	return fileSize, checksum, modTime, nil
}

// ChunkedDownload 分块下载文件，支持并行下载和 checksum 校验。
func (c *FileClient) ChunkedDownload(ctx context.Context, filename, outputPath string, opts ...ChunkedOption) error {
	params := getDownloadParams(c, opts...)
	prep, err := c.prepareChunkedDownload(ctx, filename, outputPath, params)
	if err != nil {
		return err
	}
	outputPath, fileSize, chunkSize, totalChunks, expectedChecksum, fileModTime :=
		prep.outputPath, prep.fileSize, prep.chunkSize, prep.totalChunks, prep.checksum, prep.modTime

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer outFile.Close()

	// 失败时自动清理不完整文件
	defer func() {
		if ctx.Err() != nil {
			outFile.Close()
			os.Remove(outputPath)
		}
	}()

	// 预分配空间
	if err := outFile.Truncate(fileSize); err != nil {
		return fmt.Errorf("预分配空间失败: %w", err)
	}

	// 并行下载 + 失败补拉（2026-10-23 抽离，降 gocognit）。
	if err := c.runChunkedDownload(ctx, runChunkedArgs{
		filename: filename, kind: params.kind, chunkSize: chunkSize, fileSize: fileSize,
		totalChunks: totalChunks, concurrency: params.concurrency,
		outFile: outFile, cancel: cancel,
	}); err != nil {
		os.Remove(outputPath)
		return err
	}

	// **完整性兑底（2026-10-23）**：外部卷无 X-File-Checksum 时旧行为静默放过损坏文件。
	if err := c.verifyChunkedOutput(outputPath, fileSize, expectedChecksum); err != nil {
		cancel()
		os.Remove(outputPath)
		return err
	}

	c.restoreDownloadModTime(outputPath, fileModTime)

	c.logger.Info("分块下载完成", "file_name", outputPath)
	return nil
}

// chunkedDownloadPrep 是 ChunkedDownload 的准备结果（S107：返回值收敛）。
type chunkedDownloadPrep struct {
	outputPath  string
	fileSize    int64
	chunkSize   int64
	totalChunks int
	checksum    string
	modTime     int64
}

// prepareChunkedDownload 做路径校验/解析、远端 Stat、分块大小计算与父目录创建
// （2026-10-23 从 ChunkedDownload 抽离，降 gocognit）。
func (c *FileClient) prepareChunkedDownload(ctx context.Context, filename, outputPath string, params *downloadParams) (*chunkedDownloadPrep, error) {
	if containsPathTraversal(filename) {
		return nil, fmt.Errorf("文件名不能包含路径穿越符 '..'")
	}
	resolved, err := resolveOutputPath(filename, outputPath)
	if err != nil {
		return nil, err
	}
	fileSize, checksum, modTime, err := getFileStat(ctx, c, filename, params.kind)
	if err != nil {
		return nil, err
	}
	chunkSize := calcChunkSize(fileSize, params.chunkSize, params.maxChunk)
	if mkdirErr := os.MkdirAll(filepath.Dir(resolved), 0o755); mkdirErr != nil {
		return nil, fmt.Errorf("创建父目录失败: %w", mkdirErr)
	}
	return &chunkedDownloadPrep{
		outputPath:  resolved,
		fileSize:    fileSize,
		chunkSize:   chunkSize,
		totalChunks: int((fileSize + chunkSize - 1) / chunkSize),
		checksum:    checksum,
		modTime:     modTime,
	}, nil
}

// runChunkedArgs 是 runChunkedDownload 的参数结构体（S107：参数收敛）。
type runChunkedArgs struct {
	filename    string
	kind        string
	chunkSize   int64
	fileSize    int64
	totalChunks int
	concurrency int
	outFile     *os.File
	cancel      context.CancelFunc
}

// runChunkedDownload 并发下载全部分块，并对首轮失败的块串行补拉（2026-10-23 抽离，
// 降 gocognit）。单块失败**不取消全局**（旧实现 p.Cancel 会拖垮在途 chunk 并静默产出
// 零填充文件）；补拉后仍失败 ⇒ 返回错误由调用方 fail-closed 删文件。
func (c *FileClient) runChunkedDownload(ctx context.Context, a runChunkedArgs) error {
	var (
		mu       sync.Mutex
		progress int64
		wg       sync.WaitGroup
		failed   sync.Map // chunkIdx → struct{}（重试耗尽的 chunk）
	)
	conc := a.concurrency
	if conc <= 0 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	for i := range a.totalChunks {
		sem <- struct{}{}
		wg.Add(1)
		go func(chunkIdx int) {
			defer wg.Done()
			defer func() { <-sem }()
			c.downloadOneChunk(ctx, downloadChunkParams{
				Filename:  a.filename,
				Kind:      a.kind,
				ChunkIdx:  chunkIdx,
				ChunkSize: a.chunkSize,
				FileSize:  a.fileSize,
				OutFile:   a.outFile,
				Mu:        &mu,
				Progress:  &progress,
				Cancel:    a.cancel, Done: ctx.Done(),
				Failed: &failed,
			})
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("分块下载失败: %w", ctx.Err())
	}
	return c.retryFailedChunks(ctx, retryFailedChunksArgs{
		filename: a.filename, kind: a.kind, chunkSize: a.chunkSize, fileSize: a.fileSize,
		totalChunks: a.totalChunks, outFile: a.outFile, mu: &mu, progress: &progress,
		failed: &failed, cancel: a.cancel,
	})
}

// verifyChunkedOutput 是分块下载的完整性收尾（2026-10-23 抽离，降 gocognit）：
// 大小一致 → （无 checksum 时）大段零窗检测 → checksum 比对（有则必验）。
func (c *FileClient) verifyChunkedOutput(outputPath string, fileSize int64, expectedChecksum string) error {
	if err := verifyOutputSize(outputPath, fileSize); err != nil {
		return err
	}
	if expectedChecksum == "" {
		return verifyNoHugeZeroRuns(outputPath, fileSize)
	}
	return c.verifyDownloadChecksum(outputPath, expectedChecksum)
}

// downloadOneChunk 下载单个分块并写入文件指定偏移位置，内部包含重试逻辑。
//
// **2026-10-23 修复（静默损坏根因）**：旧实现失败后 `p.Cancel()` 取消整个下载 ctx ⇒
// 其它在途 chunk 全部 500（服务端 ctx canceled），输出文件（已 Truncate 零填充）残留
// 洞且 checksum 缺失时不删 ⇒ 静默产出损坏文件。现改为：重试耗尽 → 记录到 Failed 桶，
// **不取消全局**；主流程收尾对失败 chunk 单独补拉，仍失败才 fail-closed。
func (c *FileClient) downloadOneChunk(ctx context.Context, p downloadChunkParams) {
	urlPath := c.buildChunkURL(p)
	length := chunkLength(p)
	offset := int64(p.ChunkIdx) * int64(p.ChunkSize)
	if c.downloadChunkWithRetry(ctx, p, urlPath, offset, length) {
		return
	}
	if p.Failed != nil {
		p.Failed.Store(p.ChunkIdx, struct{}{})
		c.logger.WarnContext(ctx, "分块下载失败（待收尾重试）", "chunk", p.ChunkIdx,
			"offset", offset, "length", length)
		return
	}
	if !chunkDownloadDone(p.Done) {
		p.Cancel()
	}
}

// buildChunkURL 拼装单个分块下载请求路径（含 kind/volume 参数）。
func (c *FileClient) buildChunkURL(p downloadChunkParams) string {
	offset := int64(p.ChunkIdx) * int64(p.ChunkSize)
	length := chunkLength(p)
	urlPath := fmt.Sprintf("/download/chunk?filename=%s&offset=%d&length=%d",
		url.QueryEscape(p.Filename), offset, length)
	if p.Kind != "" {
		urlPath += "&kind=" + url.QueryEscape(p.Kind)
	} else {
		urlPath += c.volumeQueryPart()
	}
	return urlPath
}

// chunkLength 返回当前分块的实际长度（末块按文件剩余字节截断）。
func chunkLength(p downloadChunkParams) int64 {
	offset := int64(p.ChunkIdx) * int64(p.ChunkSize)
	length := p.ChunkSize
	if offset+length > p.FileSize {
		length = p.FileSize - offset
	}
	return length
}

// downloadChunkWithRetry 带退避重试下载一个分块；成功写入返回 true，失败返回 false。
func (c *FileClient) downloadChunkWithRetry(ctx context.Context, p downloadChunkParams, urlPath string, offset, length int64) bool {
	baseDelay := 500 * time.Millisecond
	for attempt := range maxRetries {
		if chunkDownloadDone(p.Done) {
			return false
		}
		data, ok := c.tryDownloadChunk(ctx, urlPath, length)
		if !ok {
			if attempt >= maxRetries-1 {
				break
			}
			if !chunkDownloadSleep(ctx, baseDelay*(1<<attempt)) {
				return false
			}
			continue
		}
		return c.writeDownloadChunk(ctx, p, data, offset)
	}
	return false
}

// chunkDownloadDone 检查下载是否已被取消（关闭则返回 true）。
func chunkDownloadDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// chunkDownloadSleep 退避休眠，ctx 取消时返回 false。
func chunkDownloadSleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// writeDownloadChunk 把分块数据写入目标文件（Mu 保护），并推进进度回调。
func (c *FileClient) writeDownloadChunk(ctx context.Context, p downloadChunkParams, data []byte, offset int64) bool {
	if chunkDownloadDone(p.Done) {
		return false
	}
	p.Mu.Lock()
	if _, writeErr := p.OutFile.WriteAt(data, offset); writeErr != nil {
		p.Mu.Unlock()
		p.Cancel()
		return false
	}
	*p.Progress += int64(len(data))
	progress := *p.Progress
	p.Mu.Unlock()
	if c.progressFn != nil {
		c.progressFn("下载", progress, p.FileSize)
	}
	return true
}

// retryFailedChunksArgs 是 retryFailedChunks 的参数结构体（S107：参数收敛）。
type retryFailedChunksArgs struct {
	filename    string
	kind        string
	chunkSize   int64
	fileSize    int64
	totalChunks int
	outFile     *os.File
	mu          *sync.Mutex
	progress    *int64
	failed      *sync.Map
	cancel      context.CancelFunc
}

// retryFailedChunks 串行补拉首轮失败的 chunk（2026-10-23，外部卷稳定性）。
// 每块独立重试 maxRetries 次；仍有失败 ⇒ 返回错误（调用方删输出文件，fail-closed）。
// 串行而非并发：失败通常是网盘瞬时拥塞，串行减少并发压力且便于定位。
func (c *FileClient) retryFailedChunks(ctx context.Context, a retryFailedChunksArgs) error {
	if a.failed == nil {
		return nil
	}
	var pending []int
	a.failed.Range(func(k, _ any) bool {
		if idx, ok := k.(int); ok {
			pending = append(pending, idx)
		}
		return true
	})
	if len(pending) == 0 {
		return nil
	}
	sort.Ints(pending)
	c.logger.Warn("分块下载：首轮存在失败分块，开始串行补拉", "failed", len(pending), "total", a.totalChunks)
	for _, idx := range pending {
		p := downloadChunkParams{
			Filename: a.filename, Kind: a.kind, ChunkIdx: idx,
			ChunkSize: a.chunkSize, FileSize: a.fileSize,
			OutFile: a.outFile, Mu: a.mu, Progress: a.progress,
			Cancel: a.cancel, Done: ctx.Done(),
		}
		urlPath := c.buildChunkURL(p)
		offset := int64(idx) * a.chunkSize
		length := chunkLength(p)
		if !c.downloadChunkWithRetry(ctx, p, urlPath, offset, length) {
			return fmt.Errorf("分块下载失败：分块 %d（offset=%d, len=%d）补拉后仍失败（首轮失败 %d 块）",
				idx, offset, length, len(pending))
		}
		a.failed.Delete(idx)
	}
	return nil
}

// verifyOutputSize 校验输出文件大小与远端声明一致（完整性兑底）。
func verifyOutputSize(outputPath string, want int64) error {
	if want <= 0 {
		return nil
	}
	fi, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("校验下载文件大小失败: %w", err)
	}
	if fi.Size() != want {
		return fmt.Errorf("下载文件大小不符：期望 %d，实际 %d", want, fi.Size())
	}
	return nil
}

// verifyNoHugeZeroRuns 采样检查输出文件是否存在**大段零填充**（2026-10-23：预分配
// Truncate 的洞暴露；外部卷无 checksum 时是静默损坏的最后防线）。逐 256KiB 窗口读，
// 任一窗口全零 ⇒ 判定损坏（正常音视频/二进制几乎不含连续 256KiB 零）。
// verifyNoHugeZeroRuns 采样检查输出文件是否存在**大段零填充**（2026-10-23：预分配
// Truncate 的洞暴露；外部卷无 checksum 时是静默损坏的最后防线）。逐 256KiB 窗口读，
// 任一窗口全零 ⇒ 判定损坏（正常音视频/二进制几乎不含连续 256KiB 零）。
//
// **size<=0 时用实际文件大小**（2026-10-23 关键修复）：流式响应（chunked
// transfer-encoding）无 Content-Length ⇒ size=-1；旧实现直接 return nil ⇒ 截断/零填充
// 完全不被检测（实测 400MB 文件只有前 248MB 有数据却「下载成功」）。
func verifyNoHugeZeroRuns(outputPath string, size int64) error {
	f, err := os.Open(outputPath)
	if err != nil {
		return fmt.Errorf("校验下载内容失败: %w", err)
	}
	defer f.Close()
	if size <= 0 {
		fi, serr := f.Stat()
		if serr != nil {
			return fmt.Errorf("校验下载内容失败: %w", serr)
		}
		size = fi.Size()
	}
	if size <= 0 {
		return nil
	}
	const win = 256 << 10
	buf := make([]byte, win)
	for off := int64(0); off < size; off += win {
		n, rerr := io.ReadFull(f, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return fmt.Errorf("读取下载文件校验（off=%d）: %w", off, rerr)
		}
		if n == win && allZero(buf) {
			return fmt.Errorf("下载文件校验失败：offset %d 起 256KiB 全零（疑似未写入区域/错位）", off)
		}
	}
	return nil
}

// allZero 判定切片全零。
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// verifyDownloadChecksum 校验下载后的文件 checksum 是否与预期一致。
func (c *FileClient) verifyDownloadChecksum(outputPath, expectedChecksum string) error {
	if expectedChecksum == "" {
		return nil
	}
	c.logger.Debug("分块下载文件校验", "file_name", outputPath, "checksum", shortid.ShortHash(expectedChecksum))
	localCS, err := calculateChecksum(outputPath)
	if err != nil {
		return fmt.Errorf("计算本地 SHA-256 失败: %w", err)
	}
	if localCS != expectedChecksum {
		return fmt.Errorf("文件校验失败: 服务端 %s, 本地 %s", expectedChecksum, localCS)
	}
	return nil
}

// restoreDownloadModTime 尝试恢复文件的修改时间（非致命错误仅记录日志）。
func (c *FileClient) restoreDownloadModTime(outputPath string, fileModTime int64) {
	if fileModTime <= 0 {
		return
	}
	modTime := time.Unix(0, fileModTime)
	if err := os.Chtimes(outputPath, modTime, modTime); err != nil {
		c.logger.Warn("设置文件时间戳失败", "file_name", outputPath, "error", err)
	}
}

// tryDownloadChunk 执行一次分块下载尝试：发请求、按需校验 X-Chunk-Checksum，返回 (data, true) 表示成功。
// 失败一律返回 (nil, false)，由调用方决定是否重试。
//
// 通过把 defer resp.Body.Close() 放到本函数边界，避免在 ChunkedDownload 的重试循环中累积 defer / 重复 Close。
func (c *FileClient) tryDownloadChunk(ctx context.Context, urlPath string, expectLength int64) ([]byte, bool) {
	resp, err := c.doRequest(ctx, "GET", urlPath, nil, nil)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false
	}

	maxRead := expectLength
	if maxRead <= 0 {
		maxRead = 1
	}
	limitReader := io.LimitReader(resp.Body, maxRead+1<<20) // expectLength + 1 MiB
	data, err := io.ReadAll(limitReader)
	if err != nil {
		return nil, false
	}
	if expectLength > 0 && int64(len(data)) != expectLength {
		// 服务端返回长度与请求不符（截断、错位），强制重试以避免写入错块。
		return nil, false
	}

	serverChunkCS := resp.Header.Get("X-Chunk-Checksum")
	if serverChunkCS != "" {
		chunkHash := sha256.Sum256(data)
		localCS := hex.EncodeToString(chunkHash[:])
		if localCS != serverChunkCS {
			return nil, false
		}
	}
	return data, true
}

// ChunkedOption 分块上传/下载的可选参数。
type ChunkedOption func(*chunkedOpts)

type chunkedOpts struct {
	chunkSize   int64
	concurrency int
	resume      bool
	kind        string
	metaExtra   map[string]any
}

// WithChunkedChunkSize 设置分块大小。
func WithChunkedChunkSize(size int64) ChunkedOption {
	return func(o *chunkedOpts) {
		o.chunkSize = size
	}
}

// WithChunkedConcurrency 设置并发数。
// 传入 0 或负数时自动调整为 1，避免无缓冲信号量导致死锁。
func WithChunkedConcurrency(n int) ChunkedOption {
	return func(o *chunkedOpts) {
		if n <= 0 {
			n = 1
		}
		o.concurrency = n
	}
}

// WithChunkedResume 启用续传模式。
func WithChunkedResume(enabled bool) ChunkedOption {
	return func(o *chunkedOpts) {
		o.resume = enabled
	}
}

// WithChunkedKind 设置下载 kind（如 "cloud_archive" 下载云任务归档）。
// kind 为空表示普通文件下载。filename 传归档名（单文件名），服务端按 kind 拼接内部目录。
func WithChunkedKind(kind string) ChunkedOption {
	return func(o *chunkedOpts) {
		o.kind = kind
	}
}

// ShouldAutoChunk 判断是否应自动启用分块模式。
func ShouldAutoChunk(fileSize int64) bool {
	return fileSize > size.AutoChunkThreshold
}
