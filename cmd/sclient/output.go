// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// OutputFormatter 是 CLI 输出格式化接口。
// 支持 Text 和 JSON 两种输出格式。
type OutputFormatter interface {
	// PrintFileList 输出文件列表。
	PrintFileList(files []client.FileInfo)
	// PrintShareList 输出分享列表。
	PrintShareList(shares []*client.ShareLink)
	// PrintStats 输出统计信息。
	PrintStats(stats *client.StatsResponse)
	// PrintConfig 输出配置信息。
	PrintConfig(cfg *client.ConfigResponse)
	// Printf 输出格式化字符串。
	Printf(format string, args ...interface{})
	// Println 输出一行。
	Println(args ...interface{})
}

// TextFormatter 是文本格式输出。
type TextFormatter struct {
	w io.Writer
}

// NewTextFormatter 创建文本格式输出器。
func NewTextFormatter(w io.Writer) *TextFormatter {
	return &TextFormatter{w: w}
}

func (f *TextFormatter) PrintFileList(files []client.FileInfo) {
	printFileList(files, f.w)
}

func (f *TextFormatter) PrintShareList(shares []*client.ShareLink) {
	if len(shares) == 0 {
		fmt.Fprintln(f.w, "暂无分享链接")
		return
	}
	fmt.Fprintf(f.w, "%-36s  %-40s  %-10s  %s\n", "TOKEN", "FILENAME", "STATUS", "DOWNLOADS")
	for _, s := range shares {
		status := "活跃"
		if s.Expired {
			status = "已过期"
		}
		downloads := fmt.Sprintf("%d/%d", s.Downloads, s.MaxDownloads)
		if s.MaxDownloads == 0 {
			downloads = fmt.Sprintf("%d/∞", s.Downloads)
		}
		shortToken := s.Token
		if len(shortToken) > 36 {
			shortToken = shortToken[:16] + "..." + shortToken[len(shortToken)-16:]
		}
		fmt.Fprintf(f.w, "%-36s  %-40s  %-10s  %s\n", shortToken, s.Filename, status, downloads)
	}
}

func (f *TextFormatter) PrintStats(stats *client.StatsResponse) {
	fmt.Fprintf(f.w, "服务器统计（自启动以来）\n")
	fmt.Fprintf(f.w, "磁盘使用:\n")
	fmt.Fprintf(f.w, "  目录:     %s\n", stats.DiskUsage.UploadsDir)
	fmt.Fprintf(f.w, "  文件数:   %d\n", stats.DiskUsage.TotalFiles)
	fmt.Fprintf(f.w, "  总大小:   %s\n", client.FormatByte(float64(stats.DiskUsage.TotalSize)))

	if stats.DiskTotal > 0 {
		usedPct := float64(stats.DiskUsed) / float64(stats.DiskTotal) * 100
		fmt.Fprintf(f.w, "  磁盘分区: %s / %s (%.1f%%)\n",
			client.FormatByte(float64(stats.DiskUsed)),
			client.FormatByte(float64(stats.DiskTotal)),
			usedPct)
	}

	fmt.Fprintf(f.w, "\n请求统计:\n")
	fmt.Fprintf(f.w, "  总请求数: %d\n", stats.RequestCounts.Total)
	fmt.Fprintf(f.w, "  2xx:      %d\n", stats.RequestCounts.Xx2)
	fmt.Fprintf(f.w, "  4xx:      %d\n", stats.RequestCounts.Xx4)
	fmt.Fprintf(f.w, "  5xx:      %d\n", stats.RequestCounts.Xx5)
	fmt.Fprintf(f.w, "  活跃连接: %d\n", stats.ActiveConns)

	fmt.Fprintf(f.w, "\n传输统计:\n")
	fmt.Fprintf(f.w, "  上传文件:   %d\n", stats.FilesUploaded)
	fmt.Fprintf(f.w, "  上传字节:   %s\n", client.FormatByte(float64(stats.BytesUploaded)))
	fmt.Fprintf(f.w, "  下载文件:   %d\n", stats.FilesDownloaded)
	fmt.Fprintf(f.w, "  下载字节:   %s\n", client.FormatByte(float64(stats.BytesDownloaded)))
	fmt.Fprintf(f.w, "  删除文件:   %d\n", stats.FilesDeleted)

	if stats.MaxStorageBytes > 0 {
		usagePct := float64(stats.StorageUsage) / float64(stats.MaxStorageBytes) * 100
		fmt.Fprintf(f.w, "\n存储限制: %s / %s (%.1f%%)\n",
			client.FormatByte(float64(stats.StorageUsage)),
			client.FormatByte(float64(stats.MaxStorageBytes)),
			usagePct)
	}
}

func (f *TextFormatter) PrintConfig(cfg *client.ConfigResponse) {
	fmt.Fprintf(f.w, "远程服务器配置:\n")
	fmt.Fprintf(f.w, "  log_level:              %s\n", cfg.LogLevel)
	fmt.Fprintf(f.w, "  log_format:             %s\n", cfg.LogFormat)
	fmt.Fprintf(f.w, "  auth_token:             %s\n", boolStr(cfg.AuthTokenSet))
	fmt.Fprintf(f.w, "  tunnel_key:             %s\n", boolStr(cfg.TunnelKeySet))
	fmt.Fprintf(f.w, "  rate_limit_requests:    %d\n", cfg.RateLimitRequests)
	fmt.Fprintf(f.w, "  rate_limit_window:      %s\n", cfg.RateLimitWindow)
	fmt.Fprintf(f.w, "  max_storage_bytes:      %d\n", cfg.MaxStorageBytes)
	fmt.Fprintf(f.w, "  chunk_size:             %d\n", cfg.ChunkSize)
	fmt.Fprintf(f.w, "  upload_session_ttl:     %s\n", cfg.UploadSessionTTL)
	fmt.Fprintf(f.w, "  versioning_enabled:     %v\n", cfg.VersioningEnabled)
	fmt.Fprintf(f.w, "  cloud_max_concurrent:   %d\n", cfg.CloudMaxConcurrent)
	fmt.Fprintf(f.w, "  addr:                   %s\n", cfg.Addr)
	fmt.Fprintf(f.w, "  uploads_dir:            %s\n", cfg.UploadsDir)
}

func (f *TextFormatter) Printf(format string, args ...interface{}) {
	fmt.Fprintf(f.w, format, args...)
}

func (f *TextFormatter) Println(args ...interface{}) {
	fmt.Fprintln(f.w, args...)
}

// JSONFormatter 是 JSON 格式输出。
type JSONFormatter struct {
	w io.Writer
}

// NewJSONFormatter 创建 JSON 格式输出器。
func NewJSONFormatter(w io.Writer) *JSONFormatter {
	return &JSONFormatter{w: w}
}

func (f *JSONFormatter) PrintFileList(files []client.FileInfo) {
	enc := json.NewEncoder(f.w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"files": files, "total": len(files)})
}

func (f *JSONFormatter) PrintShareList(shares []*client.ShareLink) {
	enc := json.NewEncoder(f.w)
	_ = enc.Encode(map[string]any{"shares": shares})
}

func (f *JSONFormatter) PrintStats(stats *client.StatsResponse) {
	enc := json.NewEncoder(f.w)
	_ = enc.Encode(stats)
}

func (f *JSONFormatter) PrintConfig(cfg *client.ConfigResponse) {
	enc := json.NewEncoder(f.w)
	_ = enc.Encode(cfg)
}

func (f *JSONFormatter) Printf(format string, args ...interface{}) {
	// JSON 模式下忽略 Printf
}

func (f *JSONFormatter) Println(args ...interface{}) {
	// JSON 模式下忽略 Println
}

// buildFormatter 根据 --json flag 创建 OutputFormatter。
func buildFormatter(cmd *cobra.Command) OutputFormatter {
	useJSON, _ := cmd.Flags().GetBool("json")
	if useJSON {
		return NewJSONFormatter(os.Stdout)
	}
	return NewTextFormatter(os.Stdout)
}

// boolStr 返回布尔值的"已设置"/"未设置"文本。
func boolStr(v bool) string {
	if v {
		return "已设置"
	}
	return "未设置"
}

// printFileList 将 FileInfo 切片格式化为表格输出到指定 writer。
func printFileList(files []client.FileInfo, w io.Writer) {
	for _, f := range files {
		if f.IsDir {
			fmt.Fprintf(w, "[DIR]  %-50s\n", f.Name+"/")
		} else {
			checksumStr := f.Checksum
			if len(checksumStr) > 16 {
				checksumStr = checksumStr[:16]
			}
			if checksumStr == "" {
				checksumStr = "-"
			}
			fmt.Fprintf(w, "       %-50s  %10s  %s\n", f.Name, client.FormatByte(float64(f.Size)), checksumStr)
		}
	}
}
