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
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
)

const (
	defaultConcurrency = 4
	maxRetries         = 3
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
type uploadCacheEntry struct {
	fileSize     int64
	modTime      time.Time
	fileChecksum string
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
		// 逐步增大分块大小，但不超过 maxChunk
		for chunkSize*512 < fileSize && chunkSize < maxChunk {
			chunkSize *= 2
		}
		if chunkSize > maxChunk {
			chunkSize = maxChunk
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

// newChunkedUploader 创建分块上传器。
func newChunkedUploader(client *FileClient, filePath, uploadID string, chunkSize, fileSize int64, totalChunks int, checksum, filename string, concurrency int) *ChunkedUploader {
	return &ChunkedUploader{
		client:      client,
		chunkSize:   chunkSize,
		concurrency: concurrency,
		fileSize:    fileSize,
		totalChunks: totalChunks,
		filePath:    filePath,
		filename:    filename,
		uploadID:    uploadID,
		checksum:    checksum,
	}
}

// run 执行分块上传循环，上传指定索引列表的分块，然后完成上传。
func (u *ChunkedUploader) run(ctx context.Context, chunkIndices []int) (*ChunkedUploadResult, error) {
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

			for range maxRetries {
				if u.failed.Load() {
					return
				}

				if u.uploadChunk(ctx, chunkIdx) {
					return // 上传成功
				}
				if u.failed.Load() {
					return // 非重试错误
				}
				// should_retry: 继续重试
			}
			// 重试耗尽
			u.client.logger.Warn("chunk 重试耗尽", "chunk_index", chunkIdx,
				"upload_id", shortid.ShortHash(u.uploadID))
			u.failed.Store(true)
		}(idx)
	}

	wg.Wait()

	if u.failed.Load() {
		return nil, fmt.Errorf("上传失败：部分分块上传失败，可使用 --resume 续传")
	}

	// 完成上传
	completeBody, _ := json.Marshal(chunkedCompleteRequest{UploadID: u.uploadID})
	resp, err := u.client.doRequest(ctx, "POST", "/upload/complete", bytes.NewReader(completeBody), http.Header{
		"Content-Type": {"application/json"},
	})
	if err != nil {
		return nil, fmt.Errorf("完成上传请求失败: %w", err)
	}
	defer resp.Body.Close()

	var completeResult ChunkedUploadResult
	if err := json.NewDecoder(resp.Body).Decode(&completeResult); err != nil {
		return nil, fmt.Errorf("解析 complete 响应失败: %w", err)
	}

	if !completeResult.Success {
		return nil, fmt.Errorf("文件合并失败: %s", completeResult.Message)
	}

	u.client.logger.Info("分块上传完成", "file_name", u.filename, "checksum", shortid.ShortHash(u.checksum))
	return &completeResult, nil
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
	body, ct, err := u.buildChunkRequest(chunkIdx, chunkData, chunkChecksum)
	if err != nil {
		u.client.logger.Warn("chunk 构建请求失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "error", err)
		return false
	}

	success, shouldRetry, statusCode, message := u.sendChunkRequest(ctx, chunkIdx, body, ct)
	if success {
		u.mu.Lock()
		u.progress += int64(n)
		if u.client.progressFn != nil {
			u.client.progressFn("上传", u.progress, u.fileSize)
		}
		u.client.logger.Debug("chunk 上传成功", "chunk_index", chunkIdx, "checksum", shortid.ShortHash(chunkChecksum))
		u.mu.Unlock()
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
	u.mu.Lock()
	f, err := os.Open(u.filePath)
	u.mu.Unlock()
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
func (u *ChunkedUploader) buildChunkRequest(chunkIdx int, chunkData []byte, chunkChecksum string) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("upload_id", u.uploadID); err != nil {
		return nil, "", fmt.Errorf("写入 upload_id: %w", err)
	}
	if err := mw.WriteField("chunk_index", fmt.Sprintf("%d", chunkIdx)); err != nil {
		return nil, "", fmt.Errorf("写入 chunk_index: %w", err)
	}
	if err := mw.WriteField("chunk_checksum", chunkChecksum); err != nil {
		return nil, "", fmt.Errorf("写入 chunk_checksum: %w", err)
	}

	part, err := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", chunkIdx))
	if err != nil {
		return nil, "", fmt.Errorf("创建 form file: %w", err)
	}
	if _, err = part.Write(chunkData); err != nil {
		return nil, "", fmt.Errorf("写入 form part: %w", err)
	}
	mw.Close()

	return &buf, mw.FormDataContentType(), nil
}

// sendChunkRequest 发送分块上传请求并解析响应，返回 success、shouldRetry、statusCode、message。
func (u *ChunkedUploader) sendChunkRequest(ctx context.Context, chunkIdx int, body io.Reader, contentType string) (success, shouldRetry bool, statusCode int, message string) {
	headers := make(http.Header)
	headers.Set("Content-Type", contentType)

	chunkResp, err := u.client.doRequest(ctx, "POST", "/upload/chunk", body, headers)
	if err != nil {
		u.client.logger.Warn("chunk 上传请求失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "error", err)
		return false, true, 0, ""
	}
	defer chunkResp.Body.Close()

	var chunkResult struct {
		Success     bool   `json:"success"`
		ShouldRetry bool   `json:"should_retry"`
		Message     string `json:"message"`
	}
	if decodeErr := json.NewDecoder(chunkResp.Body).Decode(&chunkResult); decodeErr != nil {
		u.client.logger.Warn("chunk 响应解析失败", "chunk_index", chunkIdx,
			"upload_id", shortid.ShortHash(u.uploadID), "status", chunkResp.StatusCode,
			"error", decodeErr)
		return false, true, chunkResp.StatusCode, ""
	}

	return chunkResult.Success, chunkResult.ShouldRetry, chunkResp.StatusCode, chunkResult.Message
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
		chunkSize:   c.ChunkSize,
		concurrency: defaultConcurrency,
		resume:      true,
	}
	for _, o := range opts {
		o(opt)
	}
	maxChunk := c.MaxChunkSize
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

	// 检查 checksum 缓存
	var fileChecksum string
	absPath, _ := filepath.Abs(localPath)
	if cached, ok := c.uploadCache.Load(absPath); ok {
		entry := cached.(*uploadCacheEntry) //nolint:errcheck
		if entry.fileSize == fileSize && entry.modTime.Equal(modTime) {
			fileChecksum = entry.fileChecksum
			c.logger.Debug("checksum 缓存命中", "file_path", localPath)
		}
	}

	if fileChecksum == "" {
		// 计算完整文件 SHA-256
		h := sha256.New()
		if _, err = io.Copy(h, file); err != nil {
			return nil, fmt.Errorf("计算 SHA-256 失败: %w", err)
		}
		fileChecksum = hex.EncodeToString(h.Sum(nil))
		// 写入缓存
		c.uploadCache.Store(absPath, &uploadCacheEntry{
			fileSize:     fileSize,
			modTime:      modTime,
			fileChecksum: fileChecksum,
		})
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("重置文件指针失败: %w", err)
		}
		c.logger.Debug("文件 SHA-256 计算完毕", "file_path", localPath, "checksum", shortid.ShortHash(fileChecksum))
	}

	// 自适应分块大小
	chunkSize := calcChunkSize(fileSize, opt.chunkSize, maxChunk)
	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)
	filename := filepath.ToSlash(filepath.Clean(remotePath))
	uploadID := generateUploadID(filename, fileSize, modTime, fileChecksum)

	c.logger.Info("分块上传开始", "file_name", filename, "file_size", fileSize,
		"chunk_size", chunkSize, "total_chunks", totalChunks, "upload_id", shortid.ShortHash(uploadID))

	// 当 resume=false 时，跳过续传查询，直接走新建 session 路径
	if opt.resume {
		// 统一查询：先通过 upload_id + filename 查询文件状态
		statusResp, statusErr := c.doRequest(ctx, "GET",
			fmt.Sprintf("/upload/status?upload_id=%s&filename=%s", uploadID, url.QueryEscape(filename)), nil, nil)
		if statusErr == nil && statusResp.StatusCode == http.StatusOK {
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
			if json.NewDecoder(statusResp.Body).Decode(&statusData) == nil && statusData.Success {
				statusResp.Body.Close()

				// 状态1：文件已完整上传
				if statusData.Finished || statusData.Completed {
					c.logger.Info("文件已存在，直接返回成功", "file_name", filename, "checksum", shortid.ShortHash(fileChecksum))
					return &ChunkedUploadResult{
						Success:      true,
						UploadID:     uploadID,
						Filename:     filename,
						FileChecksum: fileChecksum,
						Message:      "文件已存在",
					}, nil
				}

				// 状态2：有未完成的 session，续传
				if statusData.UploadID != "" {
					c.logger.Info("续传会话已恢复", "upload_id", shortid.ShortHash(uploadID),
						"missing", len(statusData.MissingChunks), "total", statusData.TotalChunks)

					// 只上传缺失分块
					var chunkResult *ChunkedUploadResult
					chunkResult, err = c.uploadChunks(ctx, localPath, uploadID, chunkSize, fileSize, totalChunks, fileChecksum, filename, statusData.MissingChunks, opt.concurrency)
					if err != nil {
						return nil, err
					}
					return chunkResult, nil
				}
			} else {
				statusResp.Body.Close()
			}
		} else if statusErr == nil {
			statusResp.Body.Close()
		}
	}

	// 状态3：新文件 / 不在上传中，创建新 session
	c.logger.Info("新上传", "file_name", filename, "upload_id", shortid.ShortHash(uploadID))

	initBody := chunkedInitRequest{
		UploadID:     uploadID,
		Filename:     filename,
		TotalSize:    fileSize,
		ChunkSize:    chunkSize,
		TotalChunks:  totalChunks,
		FileChecksum: fileChecksum,
		FileModTime:  modTime.UnixNano(),
	}
	initJSON, _ := json.Marshal(initBody)

	resp, err := c.doRequest(ctx, "POST", "/upload/init", bytes.NewReader(initJSON), http.Header{
		"Content-Type": {"application/json"},
	})
	if err != nil {
		return nil, fmt.Errorf("初始化上传失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("初始化上传失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var initResult struct {
		Success   bool   `json:"success"`
		UploadID  string `json:"upload_id"`
		ChunkSize int64  `json:"chunk_size"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&initResult); err != nil {
		return nil, fmt.Errorf("解析 init 响应失败: %w", err)
	}
	if !initResult.Success {
		return nil, fmt.Errorf("初始化上传失败: %s", initResult.Message)
	}

	// 如果 upload_id = "already_exists"，说明文件已存在且 checksum 匹配
	if initResult.UploadID == "already_exists" {
		c.logger.Info("文件已存在，直接返回成功", "file_name", filename)
		return &ChunkedUploadResult{
			Success:      true,
			UploadID:     "already_exists",
			Filename:     filename,
			FileChecksum: fileChecksum,
		}, nil
	}

	// 使用服务端返回的 chunk_size
	if initResult.ChunkSize > 0 {
		chunkSize = initResult.ChunkSize
		totalChunks = int((fileSize + chunkSize - 1) / chunkSize)
		c.logger.Info("服务端返回的 chunk_size", "chunk_size", chunkSize, "total_chunks", totalChunks)
	}

	// 上传全部分块
	allChunks := make([]int, totalChunks)
	for i := 0; i < totalChunks; i++ {
		allChunks[i] = i
	}
	return c.uploadChunks(ctx, localPath, uploadID, chunkSize, fileSize, totalChunks, fileChecksum, filename, allChunks, opt.concurrency)
}

// uploadChunks 上传指定索引列表的分块，然后完成上传。
func (c *FileClient) uploadChunks(ctx context.Context, filePath, uploadID string, chunkSize, fileSize int64, totalChunks int, fileChecksum, filename string, chunkIndices []int, concurrency int) (*ChunkedUploadResult, error) {
	uploader := newChunkedUploader(c, filePath, uploadID, chunkSize, fileSize, totalChunks, fileChecksum, filename, concurrency)
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
		chunkSize:   c.ChunkSize,
		concurrency: defaultConcurrency,
	}
	for _, o := range opts {
		o(opt)
	}
	maxChunk := c.MaxChunkSize
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
		checksum = statResp.Header.Get("X-File-Checksum")
		if m := statResp.Header.Get("X-File-MTime"); m != "" {
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
	}

	// 获取文件信息（直接 Stat）
	fileSize, expectedChecksum, fileModTime, err := getFileStat(ctx, c, filename)
	if err != nil {
		return err
	}

	chunkSize := calcChunkSize(fileSize, params.chunkSize, params.maxChunk)
	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)

	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer outFile.Close()

	// 预分配空间
	if err := outFile.Truncate(fileSize); err != nil {
		return fmt.Errorf("预分配空间失败: %w", err)
	}

	var (
		mu          sync.Mutex
		progress    int64
		downloadErr error
		wg          sync.WaitGroup
	)

	sem := make(chan struct{}, params.concurrency)

	for i := range totalChunks {
		sem <- struct{}{}
		wg.Add(1)

		go func(chunkIdx int) {
			defer wg.Done()
			defer func() { <-sem }()

			offset := int64(chunkIdx) * int64(chunkSize)
			length := chunkSize
			if offset+length > fileSize {
				length = fileSize - offset
			}

			urlPath := fmt.Sprintf("/download/chunk?filename=%s&offset=%d&length=%d",
				url.QueryEscape(filename), offset, length)

			for range maxRetries {
				if downloadErr != nil {
					return
				}

				data, ok := c.tryDownloadChunk(ctx, urlPath, length)
				if !ok {
					continue
				}

				mu.Lock()
				if _, writeErr := outFile.WriteAt(data, offset); writeErr != nil {
					mu.Unlock()
					continue
				}
				progress += int64(len(data))
				if c.progressFn != nil {
					c.progressFn("下载", progress, fileSize)
				}
				mu.Unlock()
				return
			}

			mu.Lock()
			if downloadErr == nil {
				downloadErr = fmt.Errorf("分块 %d 下载失败（重试耗尽）", chunkIdx)
			}
			mu.Unlock()
		}(i)
	}

	wg.Wait()

	if downloadErr != nil {
		os.Remove(outputPath)
		return downloadErr
	}

	// 校验完整文件 checksum
	if expectedChecksum != "" {
		c.logger.Debug("分块下载文件校验", "file_name", outputPath, "checksum", shortid.ShortHash(expectedChecksum))
		localCS, err := calculateChecksum(outputPath)
		if err != nil {
			return fmt.Errorf("计算本地 SHA-256 失败: %w", err)
		}
		if localCS != expectedChecksum {
			return fmt.Errorf("文件校验失败: 服务端 %s, 本地 %s", expectedChecksum, localCS)
		}
	}

	// 恢复文件修改时间
	if fileModTime > 0 {
		modTime := time.Unix(0, fileModTime)
		if err := os.Chtimes(outputPath, modTime, modTime); err != nil {
			c.logger.Warn("设置文件时间戳失败", "file_name", outputPath, "error", err)
		}
	}

	c.logger.Info("分块下载完成", "file_name", outputPath)
	return nil
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

	data, err := io.ReadAll(resp.Body)
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
func WithChunkedConcurrency(n int) ChunkedOption {
	return func(o *chunkedOpts) {
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
