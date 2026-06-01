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
	"sync"
	"time"
)

const (
	defaultChunkSize   = 4 << 20  // 4 MiB
	defaultMaxChunk    = 64 << 20 // 64 MiB
	defaultConcurrency = 4
	maxRetries         = 3
	autoChunkThreshold = 100 << 20 // 100 MiB：超过此大小自动启用分块
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

var uploadCache sync.Map // key = absFilePath

// calcChunkSize 根据文件大小自适应计算分块大小。
// preferred 为首选分块大小（默认 4 MiB），maxChunk 为最大限制（默认 64 MiB）。
func calcChunkSize(fileSize, preferred, maxChunk int64) int64 {
	if maxChunk <= 0 {
		maxChunk = defaultMaxChunk
	}
	if preferred <= 0 {
		preferred = defaultChunkSize
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
		maxChunk = defaultMaxChunk
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
	if cached, ok := uploadCache.Load(absPath); ok {
		entry := cached.(*uploadCacheEntry)
		if entry.fileSize == fileSize && entry.modTime.Equal(modTime) {
			fileChecksum = entry.fileChecksum
			c.logger.Debug("checksum 缓存命中", "filepath", localPath)
		}
	}

	if fileChecksum == "" {
		// 计算完整文件 SHA-256
		h := sha256.New()
		if _, err := io.Copy(h, file); err != nil {
			return nil, fmt.Errorf("计算 SHA-256 失败: %w", err)
		}
		fileChecksum = hex.EncodeToString(h.Sum(nil))
		// 写入缓存
		uploadCache.Store(absPath, &uploadCacheEntry{
			fileSize:     fileSize,
			modTime:      modTime,
			fileChecksum: fileChecksum,
		})
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("重置文件指针失败: %w", err)
		}
		c.logger.Debug("文件 SHA-256 计算完毕", "filepath", localPath, "checksum", shortHash(fileChecksum))
	}

	// 自适应分块大小
	chunkSize := calcChunkSize(fileSize, opt.chunkSize, maxChunk)
	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)
	filename := filepath.ToSlash(filepath.Clean(remotePath))
	uploadID := generateUploadID(filename, fileSize, modTime, fileChecksum)

	c.logger.Info("分块上传开始", "filename", filename, "fileSize", fileSize,
		"chunkSize", chunkSize, "totalChunks", totalChunks, "upload_id", shortHash(uploadID))

	// 统一查询：先通过 upload_id + filename 查询文件状态
	statusResp, err := c.doRequest(ctx, "GET",
		fmt.Sprintf("/upload/status?upload_id=%s&filename=%s", uploadID, url.QueryEscape(filename)), nil, nil)
	if err == nil && statusResp.StatusCode == http.StatusOK {
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
				c.logger.Info("文件已存在，直接返回成功", "filename", filename, "checksum", shortHash(fileChecksum))
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
				c.logger.Info("续传会话已恢复", "upload_id", shortHash(uploadID),
					"missing", len(statusData.MissingChunks), "total", statusData.TotalChunks)

				// 只上传缺失分块
				result, err := c.uploadChunks(ctx, localPath, uploadID, chunkSize, fileSize, totalChunks, fileChecksum, filename, statusData.MissingChunks, opt.concurrency)
				if err != nil {
					return nil, err
				}
				return result, nil
			}
		} else {
			statusResp.Body.Close()
		}
	} else if err == nil {
		statusResp.Body.Close()
	}

	// 状态3：新文件 / 不在上传中，创建新 session
	c.logger.Info("新上传", "filename", filename, "upload_id", shortHash(uploadID))

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
		c.logger.Info("文件已存在，直接返回成功", "filename", filename)
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
	var (
		mu       sync.Mutex
		failed   bool
		wg       sync.WaitGroup
		progress int64
	)

	sem := make(chan struct{}, concurrency)

	type chunkTask struct {
		index int
	}

	taskCh := make(chan chunkTask, len(chunkIndices))
	for _, i := range chunkIndices {
		taskCh <- chunkTask{index: i}
	}
	close(taskCh)

	for task := range taskCh {
		if failed {
			break
		}
		idx := task.index

		sem <- struct{}{}
		wg.Add(1)

		go func(chunkIdx int) {
			defer wg.Done()
			defer func() { <-sem }()

			for range maxRetries {
				if failed {
					return
				}
				// 读取分块数据
				chunkData := make([]byte, chunkSize)
				offset := int64(chunkIdx) * int64(chunkSize)

				mu.Lock()
				f, err := os.Open(filePath)
				mu.Unlock()
				if err != nil {
					return
				}
				f.Seek(offset, io.SeekStart)
				n, readErr := io.ReadFull(f, chunkData)
				f.Close()
				if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
					continue
				}
				chunkData = chunkData[:n]

				// 计算分块 SHA-256
				chunkHash := sha256.Sum256(chunkData)
				chunkChecksum := hex.EncodeToString(chunkHash[:])

				// 构造 multipart 请求
				var buf bytes.Buffer
				mw := multipart.NewWriter(&buf)
				mw.WriteField("upload_id", uploadID)
				mw.WriteField("chunk_index", fmt.Sprintf("%d", chunkIdx))
				mw.WriteField("chunk_checksum", chunkChecksum)

				part, err := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", chunkIdx))
				if err != nil {
					continue
				}
				part.Write(chunkData)
				mw.Close()

				headers := make(http.Header)
				headers.Set("Content-Type", mw.FormDataContentType())

				chunkResp, err := c.doRequest(ctx, "POST", "/upload/chunk", &buf, headers)
				if err != nil {
					continue
				}

				var chunkResult struct {
					Success     bool   `json:"success"`
					ShouldRetry bool   `json:"should_retry"`
					Message     string `json:"message"`
				}
				json.NewDecoder(chunkResp.Body).Decode(&chunkResult)
				chunkResp.Body.Close()

				if chunkResult.Success {
					// 更新进度
					mu.Lock()
					progress += int64(n)
					if c.progressFn != nil {
						c.progressFn("上传", progress, fileSize)
					}
					c.logger.Debug("chunk 上传成功", "chunk_index", chunkIdx, "checksum", shortHash(chunkChecksum))
					mu.Unlock()
					return // 上传成功
				}

				if !chunkResult.ShouldRetry {
					// 非重试错误（如 upload_id 过期），标记失败
					mu.Lock()
					failed = true
					mu.Unlock()
					return
				}
				// should_retry: 继续重试
			}
			// 重试耗尽
			mu.Lock()
			failed = true
			mu.Unlock()
		}(idx)
	}

	wg.Wait()

	if failed {
		return nil, fmt.Errorf("上传失败：部分分块上传失败，可使用 --resume 续传")
	}

	// 完成上传
	completeBody, _ := json.Marshal(chunkedCompleteRequest{UploadID: uploadID})
	resp, err := c.doRequest(ctx, "POST", "/upload/complete", bytes.NewReader(completeBody), http.Header{
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

	c.logger.Info("分块上传完成", "filename", filename, "checksum", shortHash(fileChecksum))
	return &completeResult, nil
}

// ChunkedDownload 分块下载文件，支持并行下载和 checksum 校验。
func (c *FileClient) ChunkedDownload(ctx context.Context, filename, outputPath string, opts ...ChunkedOption) error {
	opt := &chunkedOpts{
		chunkSize:   c.ChunkSize,
		concurrency: defaultConcurrency,
	}
	for _, o := range opts {
		o(opt)
	}
	maxChunk := c.MaxChunkSize
	if maxChunk <= 0 {
		maxChunk = defaultMaxChunk
	}

	if outputPath == "" {
		outputPath = filename
	}

	// 获取文件信息
	var fileSize int64
	var expectedChecksum string
	var fileModTime int64

	listResp, err := c.doRequest(ctx, "GET", "/api/files", nil, nil)
	if err == nil {
		var listData struct {
			Files []FileInfo `json:"files"`
		}
		if json.NewDecoder(listResp.Body).Decode(&listData) == nil {
			for _, f := range listData.Files {
				if f.Name == filename {
					fileSize = f.Size
					expectedChecksum = f.Checksum
					fileModTime = f.ModTime
					break
				}
			}
		}
		listResp.Body.Close()
	}

	if fileSize <= 0 {
		return fmt.Errorf("无法获取文件信息: %s", filename)
	}

	chunkSize := calcChunkSize(fileSize, opt.chunkSize, maxChunk)
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

	sem := make(chan struct{}, opt.concurrency)

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

				resp, err := c.doRequest(ctx, "GET", urlPath, nil, nil)
				if err != nil {
					continue
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					continue
				}

				// 读取分块数据
				data, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					continue
				}

				// 校验分块 checksum
				serverChunkCS := resp.Header.Get("X-Chunk-Checksum")
				if serverChunkCS != "" && c.checkChecksum {
					chunkHash := sha256.Sum256(data)
					localCS := hex.EncodeToString(chunkHash[:])
					if localCS != serverChunkCS {
						continue
					}
				}

				// 写入文件
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
	if c.checkChecksum && expectedChecksum != "" {
		c.logger.Debug("分块下载文件校验", "filename", outputPath, "expected_checksum", shortHash(expectedChecksum))
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
			c.logger.Warn("设置文件时间戳失败", "filename", outputPath, "error", err)
		}
	}

	c.logger.Info("分块下载完成", "filename", outputPath)
	return nil
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
	return fileSize > autoChunkThreshold
}
