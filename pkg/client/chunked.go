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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
)

const (
	defaultConcurrency = 4
	maxRetries         = 3

	// UploadIDAlreadyExists 是服务端返回的特殊 upload_id 值，表示文件已存在（通过 checksum 匹配）。
	// 此值由服务端协议定义，变更需同步更新。
	UploadIDAlreadyExists = "already_exists"
)

// ChunkedUploadResult 表示分块上传的结果。
type ChunkedUploadResult struct {
	Success      bool   `json:"success"`
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename,omitempty"`
	FileChecksum string `json:"file_checksum,omitempty"`
	TotalChunks  int    `json:"total_chunks,omitempty"`
	Message      string `json:"message,omitempty"`
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
}

// downloadChunkParams 是 downloadOneChunk 的参数结构体，用于减少函数参数数量（S107）。
type downloadChunkParams struct {
	Filename  string
	ChunkIdx  int
	ChunkSize int64
	FileSize  int64
	OutFile   *os.File
	Mu        *sync.Mutex
	Progress  *int64
	Cancel    context.CancelFunc
	Done      <-chan struct{}
}

// tryResumeResult 是 tryResumeSession 的返回值类型，用于替代三返回值模式。
type tryResumeResult struct {
	result         *ChunkedUploadResult
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
	uploadID    string
	checksum    string
	failed      atomic.Bool
	mu          sync.Mutex
	progress    int64
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
	}
}

// run 执行分块上传循环，上传指定索引列表的分块，然后完成上传。
func (u *ChunkedUploader) run(ctx context.Context, chunkIndices []int) (*ChunkedUploadResult, error) {
	if u.concurrency <= 0 {
		u.concurrency = 1
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

	if u.failed.Load() {
		return nil, fmt.Errorf("上传失败：部分分块上传失败，可使用 --resume 续传")
	}

	// 完成上传
	completeBody, _ := json.Marshal(chunkedCompleteRequest{UploadID: u.uploadID})
	resp, err := u.client.doRequest(ctx, "POST", "/upload/complete", bytes.NewReader(completeBody), http.Header{
		headerContentType: {"application/json"},
	})
	if err != nil {
		return nil, fmt.Errorf("完成上传请求失败: %w", err)
	}
	defer resp.Body.Close()

	var completeResult ChunkedUploadResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&completeResult); err != nil {
		return nil, fmt.Errorf("解析 complete 响应失败: %w", err)
	}

	if !completeResult.Success {
		return nil, fmt.Errorf("文件合并失败: %s", completeResult.Message)
	}

	u.client.logger.Info("分块上传完成", "file_name", u.filename, "checksum", shortid.ShortHash(u.checksum))
	return &completeResult, nil
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
	u.client.logger.Warn("chunk 重试耗尽", "chunk_index", chunkIdx,
		"upload_id", shortid.ShortHash(u.uploadID))
	u.failed.Store(true)
}

// uploadChunk 执行一个分块的完整上传流程（打开文件、读取、构建请求、发送、解析响应）。
// 返回 true 表示上传成功，false 表示需要重试（对于不可重试的错误，内部调用 u.failed.Store(true)）。
func (u *ChunkedUploader) uploadChunk(ctx context.Context, chunkIdx int) bool {
	f, err := u.openAndSeekChunk(chunkIdx)
	if err != nil {
		u.client.logger.Warn("chunk 打开文件失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "file", u.filePath, "error", err)
		return false
	}

	offset := int64(chunkIdx) * int64(u.chunkSize)
	chunkData := make([]byte, u.chunkSize)
	n, readErr := io.ReadFull(f, chunkData)
	f.Close()
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		u.client.logger.Warn("chunk 读取失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "offset", offset, "error", readErr)
		return false
	}
	chunkData = chunkData[:n]

	// 计算分块 SHA-256
	chunkHash := sha256.Sum256(chunkData)
	chunkChecksum := hex.EncodeToString(chunkHash[:])

	// 构造 multipart 请求
	body, ct, err := u.buildChunkRequest(ctx, chunkIdx, chunkData, chunkChecksum)
	if err != nil {
		u.client.logger.Warn("chunk 构建请求失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "error", err)
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
		u.client.logger.Debug("chunk 上传成功", "chunk_index", chunkIdx, "checksum", shortid.ShortHash(chunkChecksum))
		return true
	}

	if !shouldRetry {
		// 非重试错误（如 upload_id 过期），标记失败
		u.client.logger.Warn("chunk 非重试错误", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "status", statusCode,
			"message", message)
		u.failed.Store(true)
	}
	return false
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

		select {
		case <-ctx.Done():
			pw.CloseWithError(ctx.Err())
			return
		default:
		}

		writeField := func(field, value string) bool {
			select {
			case <-ctx.Done():
				pw.CloseWithError(ctx.Err())
				return false
			default:
			}
			if err := mw.WriteField(field, value); err != nil {
				pw.CloseWithError(fmt.Errorf("写入 %s: %w", field, err))
				return false
			}
			return true
		}

		if !writeField("upload_id", u.uploadID) {
			return
		}
		if !writeField("chunk_index", fmt.Sprintf("%d", chunkIdx)) {
			return
		}
		if !writeField("chunk_checksum", chunkChecksum) {
			return
		}

		select {
		case <-ctx.Done():
			pw.CloseWithError(ctx.Err())
			return
		default:
		}

		part, err := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", chunkIdx))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("创建 form file: %w", err))
			return
		}
		type writeResult struct {
			n   int
			err error
		}
		writeCh := make(chan writeResult, 1)
		go func() {
			n, werr := part.Write(chunkData)
			writeCh <- writeResult{n, werr}
		}()
		select {
		case <-ctx.Done():
			pw.CloseWithError(ctx.Err())
		case wr := <-writeCh:
			if wr.err != nil {
				pw.CloseWithError(fmt.Errorf("写入 form part: %w", wr.err))
			}
		}
	}()

	return pr, mw.FormDataContentType(), nil
}

// sendChunkRequest 发送分块上传请求并解析响应，返回 success、shouldRetry、statusCode、message。
func (u *ChunkedUploader) sendChunkRequest(ctx context.Context, chunkIdx int, body io.Reader, contentType string) (success, shouldRetry bool, statusCode int, message string) {
	headers := make(http.Header)
	headers.Set(headerContentType, contentType)

	chunkResp, err := u.client.doRequest(ctx, "POST", "/upload/chunk", body, headers)
	if err != nil {
		u.client.logger.Warn("chunk 上传请求失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "error", err)
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
		u.client.logger.Warn("chunk 响应解析失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "status", chunkResp.StatusCode,
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
		c.uploadCache.Range(func(k, v any) bool {
			entry := v.(*uploadCacheEntry) //nolint:errcheck
			if time.Since(entry.createdAt) > c.cacheTTL {
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
		c.logger.Info("文件已存在，直接返回成功", "file_name", p.Filename, "checksum", shortid.ShortHash(p.FileChecksum))
		return tryResumeResult{result: &ChunkedUploadResult{
			Success:      true,
			UploadID:     p.UploadID,
			Filename:     p.Filename,
			FileChecksum: p.FileChecksum,
			Message:      "文件已存在",
		}, shouldContinue: false}
	}

	if statusData.UploadID != "" {
		c.logger.Info("续传会话已恢复", "upload_id", shortid.ShortHash(p.UploadID),
			"missing", len(statusData.MissingChunks), "total", statusData.TotalChunks)
		result, err := c.uploadChunks(ctx, statusData.MissingChunks, chunkUploadOpts{
			filePath:     p.LocalPath,
			uploadID:     p.UploadID,
			chunkSize:    p.ChunkSize,
			fileSize:     p.FileSize,
			totalChunks:  p.TotalChunks,
			fileChecksum: p.FileChecksum,
			filename:     p.Filename,
			concurrency:  p.Concurrency,
		})
		return tryResumeResult{result: result, err: err, shouldContinue: false}
	}

	return tryResumeResult{shouldContinue: true}
}

// initNewUploadSession 创建新的上传 session，并返回服务端 chunk_size。
func (c *FileClient) initNewUploadSession(ctx context.Context, p resumeSessionParams) (int64, int, error) {
	initBody := chunkedInitRequest{
		UploadID:     p.UploadID,
		Filename:     p.Filename,
		TotalSize:    p.FileSize,
		ChunkSize:    p.ChunkSize,
		TotalChunks:  p.TotalChunks,
		FileChecksum: p.FileChecksum,
		FileModTime:  p.ModTime.UnixNano(),
	}
	initJSON, _ := json.Marshal(initBody)

	resp, err := c.doRequest(ctx, "POST", "/upload/init", bytes.NewReader(initJSON), http.Header{
		headerContentType: {"application/json"},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("初始化上传失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return 0, 0, fmt.Errorf("初始化上传失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var initResult struct {
		Success   bool   `json:"success"`
		UploadID  string `json:"upload_id"`
		ChunkSize int64  `json:"chunk_size"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&initResult); err != nil {
		return 0, 0, fmt.Errorf("解析 init 响应失败: %w", err)
	}
	if !initResult.Success {
		return 0, 0, fmt.Errorf("初始化上传失败: %s", initResult.Message)
	}

	// 如果 upload_id = "already_exists"，说明文件已存在且 checksum 匹配
	if initResult.UploadID == UploadIDAlreadyExists {
		return 0, -1, nil // -1 表示已存在
	}

	newChunkSize := p.ChunkSize
	if initResult.ChunkSize > 0 {
		newChunkSize = initResult.ChunkSize
	}
	newTotalChunks := int((p.FileSize + newChunkSize - 1) / newChunkSize)
	return newChunkSize, newTotalChunks, nil
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

	c.logger.Info("分块上传开始", "file_name", filename, "file_size", fileSize,
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
		})
		if !res.shouldContinue {
			return res.result, res.err
		}
	}

	// 新文件 / 不在上传中，创建新 session
	c.logger.Info("新上传", "file_name", filename, "upload_id", shortid.ShortHash(uploadID))

	newChunkSize, newTotalChunks, err := c.initNewUploadSession(ctx, resumeSessionParams{
		UploadID:     uploadID,
		Filename:     filename,
		FileChecksum: fileChecksum,
		FileSize:     fileSize,
		ChunkSize:    chunkSize,
		TotalChunks:  totalChunks,
		ModTime:      modTime,
	})
	if err != nil {
		return nil, err
	}
	if newTotalChunks == -1 {
		c.logger.Info("文件已存在，直接返回成功", "file_name", filename)
		return &ChunkedUploadResult{
			Success:      true,
			UploadID:     UploadIDAlreadyExists,
			Filename:     filename,
			FileChecksum: fileChecksum,
		}, nil
	}
	if newChunkSize != chunkSize {
		chunkSize = newChunkSize
		totalChunks = newTotalChunks
		c.logger.Info("服务端返回的 chunk_size", "chunk_size", chunkSize, "total_chunks", totalChunks)
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
	})
	return uploader.run(ctx, chunkIndices)
}

// downloadParams 分块下载的参数。
type downloadParams struct {
	chunkSize   int64
	concurrency int
	maxChunk    int64
}

// getDownloadParams 解析分块下载的选项参数。
func getDownloadParams(c *FileClient, opts ...ChunkedOption) *downloadParams {
	opt := &chunkedOpts{
		chunkSize:   c.chunkSize,
		concurrency: defaultConcurrency,
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
	}
}

// getFileStat 通过 HEAD 请求获取远端文件的元信息。
func getFileStat(ctx context.Context, c *FileClient, filename string) (fileSize int64, checksum string, modTime int64, err error) {
	statResp, err := c.doRequest(ctx, "HEAD", "/api/files/stat?filename="+url.QueryEscape(filename), nil, nil)
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

	if outputPath == "" {
		outputPath = filename
		if strings.Contains(filepath.Clean(outputPath), "..") {
			return fmt.Errorf("文件名不能包含路径穿越符 '..'")
		}
	} else {
		// 非空 outputPath 也做检查
		if err := validateOutputPath(outputPath); err != nil {
			return err
		}
	}

	// 获取文件信息（直接 Stat）
	fileSize, expectedChecksum, fileModTime, err := getFileStat(ctx, c, filename)
	if err != nil {
		return err
	}

	chunkSize := calcChunkSize(fileSize, params.chunkSize, params.maxChunk)
	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)

	// 创建父目录（如果不存在）
	if mkdirErr := os.MkdirAll(filepath.Dir(outputPath), 0755); mkdirErr != nil {
		return fmt.Errorf("创建父目录失败: %w", mkdirErr)
	}

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

	var (
		mu       sync.Mutex
		progress int64
		wg       sync.WaitGroup
	)

	if params.concurrency <= 0 {
		params.concurrency = 1
	}
	sem := make(chan struct{}, params.concurrency)

	for i := range totalChunks {
		sem <- struct{}{}
		wg.Add(1)

		go func(chunkIdx int) {
			defer wg.Done()
			defer func() { <-sem }()
			c.downloadOneChunk(ctx, downloadChunkParams{
				Filename:  filename,
				ChunkIdx:  chunkIdx,
				ChunkSize: chunkSize,
				FileSize:  fileSize,
				OutFile:   outFile,
				Mu:        &mu,
				Progress:  &progress,
				Cancel:    cancel, Done: ctx.Done(),
			})
		}(i)
	}

	wg.Wait()

	if ctx.Err() != nil {
		os.Remove(outputPath)
		return fmt.Errorf("分块下载失败: %w", ctx.Err())
	}

	if err := c.verifyDownloadChecksum(outputPath, expectedChecksum); err != nil {
		return err
	}

	c.restoreDownloadModTime(outputPath, fileModTime)

	c.logger.Info("分块下载完成", "file_name", outputPath)
	return nil
}

// downloadOneChunk 下载单个分块并写入文件指定偏移位置，内部包含重试逻辑。
func (c *FileClient) downloadOneChunk(ctx context.Context, p downloadChunkParams) {
	offset := int64(p.ChunkIdx) * int64(p.ChunkSize)
	length := p.ChunkSize
	if offset+length > p.FileSize {
		length = p.FileSize - offset
	}

	urlPath := fmt.Sprintf("/download/chunk?filename=%s&offset=%d&length=%d",
		url.QueryEscape(p.Filename), offset, length)

	baseDelay := 500 * time.Millisecond

	for attempt := range maxRetries {
		select {
		case <-p.Done:
			return
		default:
		}

		data, ok := c.tryDownloadChunk(ctx, urlPath, length)
		if !ok {
			if attempt < maxRetries-1 {
				delay := baseDelay * (1 << attempt)
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}
			continue
		}

		select {
		case <-p.Done:
			return
		default:
		}

		p.Mu.Lock()
		if _, writeErr := p.OutFile.WriteAt(data, offset); writeErr != nil {
			p.Mu.Unlock()
			p.Cancel()
			return
		}
		*p.Progress += int64(len(data))
		progress := *p.Progress
		p.Mu.Unlock()
		if c.progressFn != nil {
			c.progressFn("下载", progress, p.FileSize)
		}
		return
	}

	p.Cancel()
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

// ShouldAutoChunk 判断是否应自动启用分块模式。
func ShouldAutoChunk(fileSize int64) bool {
	return fileSize > size.AutoChunkThreshold
}
