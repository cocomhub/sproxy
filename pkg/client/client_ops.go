// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// client_ops.go 是 SDK 的**文件操作面**：Upload / Download（含 CloudArchive）/ Delete /
// Rename / Stat / List / ListWithPagination / Search / Mkdir / Rmdir / BatchDelete / BatchRename，
// 以及配套 DTO、checksum 计算与输出路径解析。
//
// 拆分说明见 client.go 顶部。

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
)

// calculateChecksum 计算文件的 SHA-256 十六进制摘要（无缓存版本）。
// 与 calcFileChecksum（带缓存，位于 chunked.go）不同，此函数每次调用都重新计算。
func calculateChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Upload 上传本地文件到 sproxy 服务端的指定远端路径。
//
// localPath 为本地文件路径，remotePath 为远端路径（如 "dir1/file.txt"），保留目录结构。
// 如果启用了 checksum 校验（默认开启），会在上传前计算文件的 SHA-256，
// 并通过 X-File-Checksum 请求头发送给服务端进行完整性校验。
// 同时通过 X-File-MTime 请求头传递文件的修改时间。
// 如果配置了 tunnel_key，上传数据将通过加密隧道传输。
//
// 设计说明：X-File-Checksum 请求头必须在 doRequest 调用前设置，而 body 的 SHA-256
// 需要在 multipart 写入过程中流式计算。标准库的 net/http 在发送请求时先发 header 再发 body，
// 因此无法在 body 流式写入的同时获取 checksum 并设置 header。io.TeeReader 方案在此不适用。
// 对于 ≤ 100 MiB 的文件推荐使用本方法。超过 100 MiB 的大文件请使用 ChunkedUpload
// 分块上传以获得更好的性能与并发控制。Upload 不会自动委派到 ChunkedUpload。
func (c *FileClient) Upload(ctx context.Context, localPath, remotePath string) (*UploadResult, error) {
	if remotePath == "" {
		return nil, fmt.Errorf("remotePath 不能为空")
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

	var fileChecksum string
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return nil, fmt.Errorf("计算 SHA-256 失败: %w", err)
	}
	fileChecksum = hex.EncodeToString(h.Sum(nil))
	c.logger.DebugContext(ctx, "文件 SHA-256", "file_path", localPath, "remote_path", remotePath, "checksum", shortid.ShortHash(fileChecksum))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	remoteClean := filepath.ToSlash(filepath.Clean(remotePath))
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	var uploadWg sync.WaitGroup
	uploadWg.Go(func() {
		defer pw.Close()
		defer mw.Close()
		select {
		case <-ctx.Done():
			pw.CloseWithError(ctx.Err())
			return
		default:
		}
		if c.volume != "" {
			if vErr := mw.WriteField("volume", c.volume); vErr != nil {
				pw.CloseWithError(fmt.Errorf("写入 volume 字段: %w", vErr))
				return
			}
		}
		part, wErr := mw.CreateFormFile("file", remoteClean)
		if wErr != nil {
			pw.CloseWithError(wErr)
			return
		}
		var src io.Reader = file
		if c.progressFn != nil {
			c.progressFn("上传", 0, fileSize)
			src = NewProgressReader(file, fileSize, func(read, total int64) {
				c.progressFn("上传", read, total)
			})
		}
		if _, copyErr := io.Copy(part, src); copyErr != nil {
			pw.CloseWithError(copyErr)
			return
		}
	})

	headers := make(http.Header)
	headers.Set(headerContentType, mw.FormDataContentType())
	headers.Set(headerFileChecksum, fileChecksum)
	headers.Set("X-File-Path", remoteClean)
	headers.Set(headerFileMTime, fmt.Sprintf("%d", stat.ModTime().UnixNano()))

	resp, err := c.doRequest(ctx, "POST", "/upload", pr, headers)
	uploadWg.Wait()
	if err != nil {
		pr.Close()
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("上传失败 (HTTP %d): %s", resp.StatusCode, string(body))
		// 存储不足（HTTP 507）映射为 ErrStorageFull 哨兵错误，供调用方 errors.Is 精确判断
		// （与 doRequest 的 507 映射一致；上传是对 507 做业务响应的路径，需在此补映射）。
		if resp.StatusCode == http.StatusInsufficientStorage {
			return nil, fmt.Errorf("%w: %s", ErrStorageFull, err.Error())
		}
		return nil, err
	}

	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}
	if v := resp.Header.Get(headerVolume); v != "" {
		result.Volume = v
	}

	if !result.Success {
		return &result, fmt.Errorf("上传失败: %s", result.Message)
	}

	return &result, nil
}

// Mkdir 在服务端创建指定子目录。
func (c *FileClient) Mkdir(ctx context.Context, dirname string) error {
	if containsPathTraversal(dirname) {
		return fmt.Errorf("dirname 不能包含路径穿越符 '..'")
	}
	urlPath := "/mkdir?dirname=" + url.QueryEscape(dirname)
	resp, err := c.doRequest(ctx, "POST", urlPath, nil, nil)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("创建目录失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("创建目录失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// Rmdir 在服务端删除指定目录（含所有内容）。
func (c *FileClient) Rmdir(ctx context.Context, dirname string) error {
	if containsPathTraversal(dirname) {
		return fmt.Errorf("dirname 不能包含路径穿越符 '..'")
	}
	urlPath := "/rmdir?dirname=" + url.QueryEscape(dirname)
	resp, err := c.doRequest(ctx, "POST", urlPath, nil, nil)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("删除目录失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("删除目录失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// Download 从 sproxy 服务端下载文件并保存到本地。
//
// outputPath 指定本地保存路径；为空时使用 filename。
// 如果启用了 checksum 校验（默认开启），会在下载后验证服务端返回的 X-File-Checksum。
// 如果配置了 tunnel_key，下载数据将通过加密隧道传输。

// resolveOutputPath 解析输出路径：outputPath 为空时使用 filename 作为默认值，否则校验 outputPath。
func resolveOutputPath(filename, outputPath string) (string, error) {
	if outputPath == "" {
		outputPath = filename
		if containsPathTraversal(filepath.Clean(outputPath)) {
			return "", fmt.Errorf("文件名不能包含路径穿越符 '..'")
		}
		return outputPath, nil
	}
	if err := validateOutputPath(outputPath); err != nil {
		return "", err
	}
	return outputPath, nil
}

// validateOutputPath 校验输出路径，防止路径穿越。
func validateOutputPath(path string) error {
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return fmt.Errorf("输出路径不能为空")
	}
	if containsPathTraversal(cleaned) {
		return fmt.Errorf("输出路径不能包含路径穿越符 '..'")
	}
	return nil
}

// Download 从 sproxy 服务端下载文件并保存到本地。
func (c *FileClient) Download(ctx context.Context, filename, outputPath string) error {
	return c.downloadTo(ctx, filename, outputPath, "")
}

// DownloadCloudArchive 下载云任务归档文件（kind=cloud_archive）。
// name 为归档名（单文件名，如 "x.tar.gz"），服务端按 owner 在租户 archive 桶内拼接。
func (c *FileClient) DownloadCloudArchive(ctx context.Context, name, outputPath string) error {
	return c.downloadTo(ctx, name, outputPath, DownloadKindCloudArchive)
}

// downloadTo 是 Download / DownloadCloudArchive 的公共实现。
// 大文件（超过自动分块阈值）优先走分块下载（断点续传/并发/逐块校验语义），
// 普通小文件走全量 /download（审查 #11：先前恒走全量，cloud_task/cloud_archive
// 大文件无分块与进度）。
func (c *FileClient) downloadTo(ctx context.Context, filename, outputPath, kind string) error {
	if containsPathTraversal(filename) {
		return fmt.Errorf("filename 不能包含路径穿越符 '..'")
	}
	outputPath, err := resolveOutputPath(filename, outputPath)
	if err != nil {
		return err
	}
	// 预取文件大小，超出自动分块阈值走 ChunkedDownload（kind 同步透传）
	fileSize, _, _, statErr := getFileStat(ctx, c, filename, kind)
	if statErr == nil && ShouldAutoChunk(fileSize) {
		return c.ChunkedDownload(ctx, filename, outputPath, WithChunkedKind(kind))
	}
	query := url.Values{"filename": {filename}}
	if kind != "" {
		query.Set("kind", kind)
	} else {
		c.appendVolumeQuery(query)
	}
	urlPath := "/download?" + query.Encode()
	headers := make(http.Header)

	resp, err := c.doRequest(ctx, "GET", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("下载失败 (状态码: %d): %s", resp.StatusCode, string(body))
	}

	// 从响应解析收到的 checksum（服务端在 X-File-Checksum 返回）
	serverCS := resp.Header.Get(headerFileChecksum)
	contentLength := resp.ContentLength

	// 创建父目录（如果不存在）
	if ensureErr := ensureParentDir(outputPath); ensureErr != nil {
		return fmt.Errorf("创建输出目录失败: %w", ensureErr)
	}
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer out.Close()

	var src io.Reader = resp.Body
	if c.progressFn != nil {
		c.progressFn("下载", 0, contentLength)
		src = NewProgressReader(resp.Body, contentLength, func(read, total int64) {
			c.progressFn("下载", read, total)
		})
	}
	if _, err := io.Copy(out, src); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	if serverCS != "" {
		if err := c.verifyChecksumAfterDownload(outputPath, serverCS); err != nil {
			os.Remove(outputPath)
			return fmt.Errorf("校验和验证失败: %w", err)
		}
	}

	// 恢复文件修改时间
	c.restoreFileMTimeAfterDownload(outputPath, resp)

	return nil
}

// verifyChecksumAfterDownload 验证下载文件的 SHA-256 与服务端返回的一致。
func (c *FileClient) verifyChecksumAfterDownload(outputPath, serverCS string) error {
	c.logger.Debug("下载文件校验", "file_name", outputPath, "checksum", serverCS)
	localCS, err := calculateChecksum(outputPath)
	if err != nil {
		return fmt.Errorf("计算本地 SHA-256 失败: %w", err)
	}
	if serverCS != localCS {
		return fmt.Errorf("文件校验失败: 服务端 %s, 本地 %s", serverCS, localCS)
	}
	c.logger.Debug("文件校验通过", "checksum", serverCS)
	return nil
}

// restoreFileMTimeAfterDownload 从响应头恢复下载文件的修改时间。
func (c *FileClient) restoreFileMTimeAfterDownload(outputPath string, resp *http.Response) {
	if mtimeStr := resp.Header.Get(headerFileMTime); mtimeStr != "" {
		var mtimeInt int64
		if _, err := fmt.Sscanf(mtimeStr, "%d", &mtimeInt); err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := os.Chtimes(outputPath, modTime, modTime); err != nil {
				c.logger.Warn("设置文件时间戳失败", "file_name", outputPath, "error", err)
			}
		}
	}
}

// Delete 从 sproxy 服务端删除文件。
//
// 默认通过 Stat 获取远端文件的 SHA-256 进行身份验证，无需本地文件。
// 如果提供了 localPath（非空），则会计算本地文件的 SHA-256 并与远端比对，一致才执行删除。
// 如果配置了 tunnel_key，删除请求将通过加密隧道传输。
func (c *FileClient) Delete(ctx context.Context, filename string, localPath string) error {
	if containsPathTraversal(filename) {
		return fmt.Errorf("文件名不能包含路径穿越符 '..'")
	}
	delQuery := url.Values{"filename": {filename}}
	c.appendVolumeQuery(delQuery)
	urlPath := "/delete?" + delQuery.Encode()
	headers := make(http.Header)

	// 先通过 Stat 获取远端 checksum
	fileChecksum := ""
	if info, statErr := c.Stat(ctx, filename); statErr == nil && info.Checksum != "" {
		fileChecksum = info.Checksum
	} else if statErr != nil {
		if errors.Is(statErr, ErrNotFound) {
			return fmt.Errorf("文件不存在: %s", filename)
		}
		return fmt.Errorf("获取文件信息失败: %w", statErr)
	} else {
		return fmt.Errorf("远端文件 checksum 为空，无法删除: %s", filename)
	}

	// 如果指定了本地文件路径，额外校验本地文件 checksum 与远端一致
	if localPath != "" {
		localCS, err := calculateChecksum(localPath)
		if err != nil {
			return fmt.Errorf("计算本地文件 SHA-256 失败: %w", err)
		}
		if localCS != fileChecksum {
			return fmt.Errorf("本地文件 SHA-256 与远端不匹配，拒绝删除（远端: %s, 本地: %s）",
				shortid.ShortHash(fileChecksum), shortid.ShortHash(localCS))
		}
		c.logger.Debug("本地文件校验通过", "local_path", localPath, "checksum", shortid.ShortHash(fileChecksum))
	}

	headers.Set(headerFileChecksum, fileChecksum)

	resp, err := c.doRequest(ctx, "POST", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("删除失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("删除失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf(errFmtParseResponse, err)
	}

	if !result.Success {
		return fmt.Errorf("删除失败: %s", result.Message)
	}

	return nil
}

// FileInfo 表示远端单个文件的元信息（与服务端 listFiles 响应对齐）。
type FileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"` // UnixNano
	IsDir    bool   `json:"is_dir"`
	// Volume 是条目所在卷名（多卷聚合列表新增字段；旧服务端/单卷无卷语义时为空，向后兼容）。
	Volume string `json:"volume"`
}

// Rename 通过 POST /rename?from=&to= 在服务端将文件从 from 移到 to。
// 与 Delete 对称，必须传入 from 的当前 SHA-256 用于校验（避免误覆盖）。
//
// fromChecksum 通常通过先调用 Stat 获取；为空时方法报错。
func (c *FileClient) Rename(ctx context.Context, from, to, fromChecksum string) error {
	if from == "" || to == "" {
		return fmt.Errorf("from / to 不能为空")
	}
	if containsPathTraversal(from) {
		return fmt.Errorf("源文件名不能包含路径穿越符 '..'")
	}
	if containsPathTraversal(to) {
		return fmt.Errorf("目标文件名不能包含路径穿越符 '..'")
	}
	if fromChecksum == "" {
		return fmt.Errorf("fromChecksum 不能为空（必须传入源文件 SHA-256 以防误覆盖）")
	}

	renameQuery := url.Values{"from": {from}, "to": {to}}
	c.appendVolumeQuery(renameQuery)
	urlPath := "/rename?" + renameQuery.Encode()
	headers := make(http.Header)
	headers.Set(headerFileChecksum, fromChecksum)

	resp, err := c.doRequest(ctx, "POST", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("重命名失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return fmt.Errorf("重命名失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// Stat 通过 HEAD /api/files/stat?filename=<name> 获取远端单个文件元信息。
// 响应来源于 X-File-Size、X-File-Checksum、X-File-MTime 三个响应头；不返回 body。
// 文件不存在时返回错误（HTTP 404 包装为 error）。
func (c *FileClient) Stat(ctx context.Context, filename string) (*FileInfo, error) {
	if filename == "" {
		return nil, fmt.Errorf("filename 不能为空")
	}
	if containsPathTraversal(filename) {
		return nil, fmt.Errorf("文件名不能包含路径穿越符 '..'")
	}
	statQuery := url.Values{"filename": {filename}}
	c.appendVolumeQuery(statQuery)
	urlPath := "/api/files/stat?" + statQuery.Encode()
	resp, err := c.doRequest(ctx, "HEAD", urlPath, nil, nil)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, filename)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stat 失败 (HTTP %d)", resp.StatusCode)
	}

	info := &FileInfo{
		Name:     filename,
		Checksum: resp.Header.Get(headerFileChecksum),
		IsDir:    resp.Header.Get("X-File-IsDir") == "true",
	}
	if s := resp.Header.Get("X-File-Size"); s != "" {
		_, _ = fmt.Sscanf(s, "%d", &info.Size)
	}
	if s := resp.Header.Get(headerFileMTime); s != "" {
		_, _ = fmt.Sscanf(s, "%d", &info.ModTime)
	}
	return info, nil
}

// buildSubdirPath 将子目录参数拼接为路径，并检查路径穿越。
// 返回 URL 编码后的路径字符串，可用于 URL query 参数。
func (c *FileClient) buildSubdirPath(subdirs []string) (string, error) {
	subdir := path.Join(append([]string{"/"}, subdirs...)...)
	if containsPathTraversal(subdir) {
		return "", fmt.Errorf("路径不能包含 '..'")
	}
	return url.QueryEscape(subdir), nil
}

// List 列出 sproxy 服务端上的文件，返回 name + size + checksum 的结构化列表。
//
// 支持可选的 offset 和 limit 分页参数。limit 为 0 表示不限制，offset 默认从 0 开始。
// 如果配置了 tunnel_key，列表请求将通过加密隧道传输。
func (c *FileClient) List(ctx context.Context, subdirs ...string) ([]FileInfo, error) {
	headers := make(http.Header)
	subdir, err := c.buildSubdirPath(subdirs)
	if err != nil {
		return nil, err
	}
	resp, err := c.doRequest(ctx, "GET", "/api/files?subdir="+subdir+c.volumeQueryPart(), nil, headers)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("列出文件失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}

	return result.Files, nil
}

// ListWithPagination 列出文件并返回分页信息。
// offset 从 0 开始，limit 为 0 表示不限制。
func (c *FileClient) ListWithPagination(ctx context.Context, offset, limit int, subdirs ...string) ([]FileInfo, int, error) {
	headers := make(http.Header)
	subdir, err := c.buildSubdirPath(subdirs)
	if err != nil {
		return nil, 0, err
	}
	urlPath := fmt.Sprintf("/api/files?subdir=%s&offset=%d&limit=%d", subdir, offset, limit) + c.volumeQueryPart()
	resp, err := c.doRequest(ctx, "GET", urlPath, nil, headers)
	if err != nil {
		return nil, 0, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, 0, fmt.Errorf("列出文件失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files  []FileInfo `json:"files"`
		Total  int        `json:"total"`
		Offset int        `json:"offset"`
		Limit  int        `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf(errFmtParseResponse, err)
	}

	return result.Files, result.Total, nil
}

// Search 搜索服务端文件名包含 q 的文件（不区分大小写）。
// 使用 GET /api/files/search?q=<keyword>，递归搜索全部目录。
func (c *FileClient) Search(ctx context.Context, q string) ([]FileInfo, error) {
	headers := make(http.Header)
	resp, err := c.doRequest(ctx, "GET", "/api/files/search?q="+url.QueryEscape(q), nil, headers)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("搜索失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}

	return result.Files, nil
}

// BatchDeleteRequest 批量删除请求体
type BatchDeleteRequest struct {
	Files []BatchDeleteFile `json:"files"`
}

// BatchDeleteFile 单条删除文件
type BatchDeleteFile struct {
	Filename string `json:"filename"`
	Checksum string `json:"checksum"`
}

// BatchOperationResult 批量操作单条结果
type BatchOperationResult struct {
	Filename string `json:"filename"`
	Success  bool   `json:"success"`
	Message  string `json:"message"`
}

// BatchRenameRequest 批量重命名请求体
type BatchRenameRequest struct {
	Operations []BatchRenameOp `json:"operations"`
}

// BatchRenameOp 单条重命名操作
type BatchRenameOp struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Checksum string `json:"checksum"`
}

// BatchDelete 批量删除文件。继续处理模式：单条失败不影响其余。
func (c *FileClient) BatchDelete(ctx context.Context, files []BatchDeleteFile) ([]BatchOperationResult, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("批量删除: 文件列表为空")
	}
	req := BatchDeleteRequest{Files: files}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/api/batch/delete", bytes.NewReader(body), nil)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusInsufficientStorage {
			return nil, fmt.Errorf("%w: %s", ErrStorageFull, fmt.Sprintf("批量删除失败 (HTTP %d): %s", resp.StatusCode, string(body)))
		}
		return nil, fmt.Errorf("批量删除失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var result struct {
		Results []BatchOperationResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}
	return result.Results, nil
}

// BatchRename 批量重命名文件。继续处理模式：单条失败不影响其余。
func (c *FileClient) BatchRename(ctx context.Context, operations []BatchRenameOp) ([]BatchOperationResult, error) {
	req := BatchRenameRequest{Operations: operations}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	resp, err := c.doRequest(ctx, "POST", "/api/batch/rename", bytes.NewReader(body), nil)
	if err != nil {
		return nil, fmt.Errorf(errFmtRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("批量重命名失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var result struct {
		Results []BatchOperationResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf(errFmtParseResponse, err)
	}
	return result.Results, nil
}

// TunnelDo 通过加密隧道发送一个 HTTP 请求。
//
// 使用方式与标准 http.Client.Do 相同。需要先通过 WithTunnel 配置隧道密钥。
