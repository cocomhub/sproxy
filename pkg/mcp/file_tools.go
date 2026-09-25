// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcp 实现 Model Context Protocol（MCP）服务器：手写 JSON-RPC 2.0
// 协议层 + 会话状态机 + 工具分派（FileClient 薄封装），供 AI CLI
// （Claude Code / Codex 等）通过 stdio 传输暴露 sproxy 文件能力。
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// 工具名称常量（schema 字段对齐 pkg/client 的既有参数契约）。
const (
	toolReadFile      = "read_file"
	toolWriteFile     = "write_file"
	toolListFiles     = "list_files"
	toolSearch        = "search"
	toolStat          = "stat"
	toolMkdir         = "mkdir"
	toolDelete        = "delete"
	toolShareCreate   = "share_create"
	toolCloudDownload = "cloud_download_create"
)

// maxReadFileBytes 是 read_file 的内存上限（防超大文件 OOM，设计文档 §风险）。
const maxReadFileBytes = 1 << 20 // 1 MiB

// maxWriteFileBytes 是 write_file 的单次直传上限（超限走分块管线，v2 预留）。
const maxWriteFileBytes = 32 << 20 // 32 MiB

// ToolEntry 是工具定义与分派处理器的一对一登记项。
type ToolEntry struct {
	Definition ToolDefinition
	Handler    ToolHandler
}

// NewToolRegistry 创建 sproxy 文件能力的 MCP 工具注册表（9 个工具）。
// fc 为 FileClient（SproxySig 签名由调用方经 WithAccessKey 等装配）；
// volume 为可选卷上下文（空 = auto）。
func NewToolRegistry(fc *client.FileClient, volume string) *ToolRegistry {
	files := &fileTools{fc: fc, volume: volume}
	return newToolRegistry([]ToolEntry{
		{
			Definition: ToolDefinition{
				Name:        toolReadFile,
				Description: "读取指定文件的内容（文本，最大 1 MiB）。filename 为服务端相对路径，如 dir/file.txt",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"filename": map[string]any{"type": "string", "description": "服务端文件路径"},
					},
					"required": []string{"filename"},
				},
			},
			Handler: files.readFile,
		},
		{
			Definition: ToolDefinition{
				Name:        toolWriteFile,
				Description: "将文本内容写入/覆盖服务端文件（最大 32 MiB）。filename 为服务端相对路径",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"filename": map[string]any{"type": "string", "description": "服务端文件路径"},
						"data":     map[string]any{"type": "string", "description": "文件内容"},
					},
					"required": []string{"filename", "data"},
				},
			},
			Handler: files.writeFile,
		},
		{
			Definition: ToolDefinition{
				Name:        toolListFiles,
				Description: "列出服务端目录下的文件（name/size/checksum 等元信息）。subdir 为空列出根目录",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"subdir": map[string]any{"type": "string", "description": "子目录路径（可选）"},
					},
				},
			},
			Handler: files.listFiles,
		},
		{
			Definition: ToolDefinition{
				Name:        toolSearch,
				Description: "按文件名搜索服务端文件（子字符串匹配，不区分大小写）",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string", "description": "搜索关键词"},
					},
					"required": []string{"query"},
				},
			},
			Handler: files.search,
		},
		{
			Definition: ToolDefinition{
				Name:        toolStat,
				Description: "查询单个文件/目录的元信息（大小/校验和/修改时间）",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"filename": map[string]any{"type": "string", "description": "服务端文件路径"},
					},
					"required": []string{"filename"},
				},
			},
			Handler: files.stat,
		},
		{
			Definition: ToolDefinition{
				Name:        toolMkdir,
				Description: "在服务端创建目录（支持多级路径）",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"dirname": map[string]any{"type": "string", "description": "目录路径"},
					},
					"required": []string{"dirname"},
				},
			},
			Handler: files.mkdir,
		},
		{
			Definition: ToolDefinition{
				Name:        toolDelete,
				Description: "删除服务端文件（必须提供 checksum 校验防误删，先 stat 获取）",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"filename": map[string]any{"type": "string", "description": "服务端文件路径"},
						"checksum": map[string]any{"type": "string", "description": "文件 SHA-256 校验和"},
					},
					"required": []string{"filename", "checksum"},
				},
			},
			Handler: files.delete,
		},
		{
			Definition: ToolDefinition{
				Name:        toolShareCreate,
				Description: "为文件创建分享链接（默认 24 小时有效期）",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"filename": map[string]any{"type": "string", "description": "服务端文件路径"},
					},
					"required": []string{"filename"},
				},
			},
			Handler: files.shareCreate,
		},
		{
			Definition: ToolDefinition{
				Name:        toolCloudDownload,
				Description: "创建云端下载任务（URL 指向的内容下载到服务端存储）",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{"type": "string", "description": "要下载的 URL"},
					},
					"required": []string{"url"},
				},
			},
			Handler: files.cloudDownload,
		},
	})
}

// fileTools 是工具实现：全部经 FileClient 调 sproxy HTTP API（SproxySig 签名）。
type fileTools struct {
	fc     *client.FileClient
	volume string
}

// parseArgs 将原始 arguments JSON 解码到 out；缺省参数用空对象。
// 返回 -32602 语义的校验错误。
func parseArgs(args json.RawMessage, out any) error {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(args, out); err != nil {
		return fmt.Errorf("%w: invalid arguments: %s", ErrInvalidToolArgs, err)
	}
	return nil
}

// requireStrings 校验参数对象中列出的字符串字段非空（对应 inputSchema required）。
func requireStrings(m map[string]string, fields ...string) error {
	for _, f := range fields {
		if m[f] == "" {
			return fmt.Errorf("%w: missing required argument: %s", ErrInvalidToolArgs, f)
		}
	}
	return nil
}

// sha256Hex 计算文本的 SHA-256 十六进制（write_file 上传校验用）。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (t *fileTools) readFile(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename string `json:"filename"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"filename": p.Filename}, "filename"); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}

	// 先 stat 拿大小，超限拒绝（防超大文件 OOM，设计文档 §风险）。
	info, err := t.fc.Stat(ctx, p.Filename)
	if err != nil {
		return "", fmt.Errorf("stat 失败: %w", err)
	}
	if info.Size > maxReadFileBytes {
		return "", fmt.Errorf("文件 %d 字节超过 read_file 上限 %d 字节", info.Size, maxReadFileBytes)
	}

	// 经 doRequest 直连 /download 读响应体到内存（复用 FileClient 的签名/隧道管线）。
	query := url.Values{"filename": {p.Filename}}
	if t.volume != "" {
		query.Set("volume", t.volume)
	}
	resp, err := t.fc.RequestRaw(ctx, "GET", "/download?"+query.Encode(), nil, nil)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("下载失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReadFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	if len(data) > maxReadFileBytes {
		return "", fmt.Errorf("文件超过 read_file 上限 %d 字节", maxReadFileBytes)
	}
	return string(data), nil
}

func (t *fileTools) writeFile(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename string `json:"filename"`
		Data     string `json:"data"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"filename": p.Filename, "data": p.Data}, "filename", "data"); err != nil {
		return "", err
	}
	if len(p.Data) > maxWriteFileBytes {
		return "", fmt.Errorf("内容 %d 字节超过 write_file 上限 %d 字节", len(p.Data), maxWriteFileBytes)
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}

	// 复用 FileClient 的 multipart 上传管线（SproxySig 签名 + checksum 头）：
	// 把内容作为本地"文件"上传——直接调 doRequest 组装 multipart（避免磁盘临时文件）。
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		defer mw.Close()
		part, err := mw.CreateFormFile("file", cleanRemoteName(p.Filename))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, strings.NewReader(p.Data)); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()
	headers := make(http.Header)
	headers.Set("Content-Type", mw.FormDataContentType())
	headers.Set("X-File-Checksum", sha256Hex([]byte(p.Data)))
	headers.Set("X-File-Path", cleanRemoteName(p.Filename))
	headers.Set("X-File-MTime", fmt.Sprintf("%d", time.Now().UnixNano()))
	if t.volume != "" {
		headers.Set("X-Volume", t.volume)
	}
	resp, err := t.fc.RequestRaw(ctx, "POST", "/upload", pr, headers)
	if err != nil {
		return "", fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("上传失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var result struct {
		Success  bool   `json:"success"`
		Message  string `json:"message"`
		Checksum string `json:"file_checksum"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析响应失败: %w", err)
	}
	if !result.Success {
		return "", fmt.Errorf("上传失败: %s", result.Message)
	}
	return fmt.Sprintf("uploaded %s (checksum %s)", p.Filename, result.Checksum), nil
}

func (t *fileTools) listFiles(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Subdir string `json:"subdir"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}
	var subdirs []string
	if p.Subdir != "" {
		subdirs = []string{p.Subdir}
	}
	files, err := t.fc.List(ctx, subdirs...)
	if err != nil {
		return "", fmt.Errorf("列目录失败: %w", err)
	}
	b, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("序列化失败: %w", err)
	}
	return string(b), nil
}

func (t *fileTools) search(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"query": p.Query}, "query"); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}
	files, err := t.fc.Search(ctx, p.Query)
	if err != nil {
		return "", fmt.Errorf("搜索失败: %w", err)
	}
	b, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("序列化失败: %w", err)
	}
	return string(b), nil
}

func (t *fileTools) stat(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename string `json:"filename"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"filename": p.Filename}, "filename"); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}
	info, err := t.fc.Stat(ctx, p.Filename)
	if err != nil {
		return "", fmt.Errorf("stat 失败: %w", err)
	}
	b, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("序列化失败: %w", err)
	}
	return string(b), nil
}

func (t *fileTools) mkdir(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Dirname string `json:"dirname"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"dirname": p.Dirname}, "dirname"); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}
	if err := t.fc.Mkdir(ctx, p.Dirname); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}
	return fmt.Sprintf("created directory %s", p.Dirname), nil
}

func (t *fileTools) delete(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename string `json:"filename"`
		Checksum string `json:"checksum"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"filename": p.Filename, "checksum": p.Checksum}, "filename", "checksum"); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}
	// 复用 FileClient.Delete 的 checksum 身份校验（防误删）——localPath 传 p.Checksum
	// 之外的语义：FileClient.Delete(filename, localPath) 在 localPath 非空时计算本地
	// 文件 checksum 比对远端，与我们"客户端提供远端 checksum"的语义不符；直接走
	// doRequest 手动删，checksum 头由调用方显式提供（设计文档：delete 校验 checksum 防误删）。
	query := url.Values{"filename": {p.Filename}}
	if t.volume != "" {
		query.Set("volume", t.volume)
	}
	headers := make(http.Header)
	headers.Set("X-File-Checksum", p.Checksum)
	resp, err := t.fc.RequestRaw(ctx, "POST", "/delete?"+query.Encode(), nil, headers)
	if err != nil {
		return "", fmt.Errorf("删除失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("删除失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析响应失败: %w", err)
	}
	if !result.Success {
		return "", fmt.Errorf("删除失败: %s", result.Message)
	}
	return fmt.Sprintf("deleted %s", p.Filename), nil
}

func (t *fileTools) shareCreate(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename string `json:"filename"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"filename": p.Filename}, "filename"); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}
	link, err := t.fc.CreateShare(ctx, p.Filename)
	if err != nil {
		return "", fmt.Errorf("创建分享失败: %w", err)
	}
	b, err := json.Marshal(link)
	if err != nil {
		return "", fmt.Errorf("序列化失败: %w", err)
	}
	return string(b), nil
}

func (t *fileTools) cloudDownload(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := parseArgs(args, &p); err != nil {
		return "", err
	}
	if err := requireStrings(map[string]string{"url": p.URL}, "url"); err != nil {
		return "", err
	}
	if t.fc == nil {
		return "", fmt.Errorf("FileClient 未配置")
	}
	task, err := t.fc.CloudDownload(ctx, p.URL)
	if err != nil {
		return "", fmt.Errorf("云端下载创建失败: %w", err)
	}
	b, err := json.Marshal(task)
	if err != nil {
		return "", fmt.Errorf("序列化失败: %w", err)
	}
	return string(b), nil
}

// cleanRemoteName 归一化 multipart 文件名（filepath.ToSlash + Clean，
// 与 FileClient.Upload 的 remoteClean 同口径，防路径穿越）。
func cleanRemoteName(name string) string {
	return filepath.ToSlash(filepath.Clean(name))
}
