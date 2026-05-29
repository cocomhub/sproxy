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
)

const (
	defaultChunkSize   = 4 << 20 // 4 MiB
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
	Filename     string `json:"filename"`
	TotalSize    int64  `json:"total_size"`
	ChunkSize    int64  `json:"chunk_size"`
	TotalChunks  int    `json:"total_chunks"`
	FileChecksum string `json:"file_checksum"`
}

// chunkedCompleteRequest 分块上传完成请求体。
type chunkedCompleteRequest struct {
	UploadID string `json:"upload_id"`
}

// ChunkedUpload 分块上传文件。当文件较大时自动使用分块上传，支持续传。
//
// 参数：
//   - ctx: 上下文
//   - filePath: 本地文件路径
//   - opts: 可选参数（若不设置则使用 FileClient 的默认值）
//
// 可选参数通过 ChunkedOption 函数设置：
//   - WithChunkedChunkSize(size): 分块大小
//   - WithChunkedConcurrency(n): 并发数
//   - WithChunkedResume(enabled): 续传模式
func (c *FileClient) ChunkedUpload(ctx context.Context, filePath string, opts ...ChunkedOption) (*ChunkedUploadResult, error) {
	// 解析选项
	opt := &chunkedOpts{
		chunkSize:   c.ChunkSize,
		concurrency: defaultConcurrency,
		resume:      true,
	}
	for _, o := range opts {
		o(opt)
	}
	if opt.chunkSize <= 0 {
		opt.chunkSize = defaultChunkSize
	}
	if opt.concurrency <= 0 {
		opt.concurrency = defaultConcurrency
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %w", err)
	}
	fileSize := stat.Size()

	// 计算完整文件 SHA-256
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, fmt.Errorf("计算 SHA-256 失败: %w", err)
	}
	fileChecksum := hex.EncodeToString(h.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	totalChunks := int((fileSize + opt.chunkSize - 1) / opt.chunkSize)
	filename := filepath.Base(filePath)

	// 初始化上传会话
	var initResult struct {
		Success   bool   `json:"success"`
		UploadID  string `json:"upload_id"`
		ChunkSize int64  `json:"chunk_size"`
		Message   string `json:"message"`
	}

	initBody := chunkedInitRequest{
		Filename:     filename,
		TotalSize:    fileSize,
		ChunkSize:    opt.chunkSize,
		TotalChunks:  totalChunks,
		FileChecksum: fileChecksum,
	}
	initJSON, _ := json.Marshal(initBody)

	// 尝试续传：先查询是否有未完成的 session
	if opt.resume {
		statusResp, err := c.doRequest(ctx, "GET", fmt.Sprintf("/upload/status?filename=%s", url.QueryEscape(filename)), nil, nil)
		if err == nil && statusResp.StatusCode == http.StatusOK {
			var statusData struct {
				Success       bool   `json:"success"`
				UploadID      string `json:"upload_id"`
				ReceivedCount int    `json:"received_count"`
				TotalChunks   int    `json:"total_chunks"`
				MissingChunks []int  `json:"missing_chunks"`
				Completed     bool   `json:"completed"`
			}
			if json.NewDecoder(statusResp.Body).Decode(&statusData) == nil && statusData.Success && !statusData.Completed {
				// 有未完成的 session — 继续用
				initResult.UploadID = statusData.UploadID
				initResult.Success = true
				initResult.Message = fmt.Sprintf("续传会话已恢复，缺失 %d 个分块", len(statusData.MissingChunks))
				_ = statusResp.Body.Close()

				// 更新 initResult 的 ChunkSize
				initResult.ChunkSize = opt.chunkSize
			} else {
				_ = statusResp.Body.Close()
			}
		} else if err == nil {
			_ = statusResp.Body.Close()
		}

		// 如果通过 ?filename= 没查到，尝试直接用文件名查（用另一种查询方式）
		if !initResult.Success {
			// 正常走 init 流程
			initResult.Success = false
		}
	}

	if !initResult.Success {
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

		if err := json.NewDecoder(resp.Body).Decode(&initResult); err != nil {
			return nil, fmt.Errorf("解析 init 响应失败: %w", err)
		}
		if !initResult.Success {
			return nil, fmt.Errorf("初始化上传失败: %s", initResult.Message)
		}

		// 如果服务端返回了更合适的 chunk_size，使用服务端的
		if initResult.ChunkSize > 0 {
			opt.chunkSize = initResult.ChunkSize
			// 重新计算 totalChunks
			totalChunks = int((fileSize + opt.chunkSize - 1) / opt.chunkSize)
		}
	}

	// 如果 upload_id = "already_exists"，说明文件已存在且 checksum 匹配
	if initResult.UploadID == "already_exists" {
		return &ChunkedUploadResult{
			Success:      true,
			UploadID:     "already_exists",
			Filename:     filename,
			FileChecksum: fileChecksum,
		}, nil
	}

	uploadID := initResult.UploadID
	retryChunks := make(chan int, totalChunks)
	for i := 0; i < totalChunks; i++ {
		retryChunks <- i
	}
	close(retryChunks)

	var (
		mu       sync.Mutex
		failed   bool
		wg       sync.WaitGroup
		progress int64
	)

	// 并发上载分块
	sem := make(chan struct{}, opt.concurrency)

	type chunkTask struct {
		index int
	}

	taskCh := make(chan chunkTask, totalChunks)
	for i := 0; i < totalChunks; i++ {
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
				chunkData := make([]byte, opt.chunkSize)
				offset := int64(chunkIdx) * int64(opt.chunkSize)

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
	if opt.chunkSize <= 0 {
		opt.chunkSize = defaultChunkSize
	}
	if opt.concurrency <= 0 {
		opt.concurrency = defaultConcurrency
	}

	if outputPath == "" {
		outputPath = filename
	}

	// 获取文件信息：先获取 file size 和 checksum
	var fileSize int64
	var expectedChecksum string

	// 从 list 接口获取
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
					break
				}
			}
		}
		listResp.Body.Close()
	}

	if fileSize <= 0 {
		return fmt.Errorf("无法获取文件信息: %s", filename)
	}

	totalChunks := int((fileSize + opt.chunkSize - 1) / opt.chunkSize)

	// 创建输出文件
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

			offset := int64(chunkIdx) * int64(opt.chunkSize)
			length := opt.chunkSize
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

				// 校验分块 checksum（如果服务器返回了）
				serverChunkCS := resp.Header.Get("X-Chunk-Checksum")
				if serverChunkCS != "" && c.checkChecksum {
					chunkHash := sha256.Sum256(data)
					localCS := hex.EncodeToString(chunkHash[:])
					if localCS != serverChunkCS {
						continue // 重试
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
				return // success
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
		localCS, err := calculateChecksum(outputPath)
		if err != nil {
			return fmt.Errorf("计算本地 SHA-256 失败: %w", err)
		}
		if localCS != expectedChecksum {
			return fmt.Errorf("文件校验失败: 服务端 %s, 本地 %s", expectedChecksum, localCS)
		}
	}

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
