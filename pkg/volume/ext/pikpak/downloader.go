// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// DownloaderConfig 是 PikPak 下载器配置。
type DownloaderConfig struct {
	// Cli 是官方 CLI 包装（安装/执行）。
	Cli *Cli
	// API 是 REST 客户端（转存/列表/删除）。
	API *API
	// DownloadDir 是 CLI 下载落盘目录（空 = os.TempDir()/pikpak-dl）。
	DownloadDir string
	// TempSuffix 是下载中间文件后缀（默认 .download）。
	TempSuffix string
	// Timeout 是下载总超时（默认 2h；免费账号限速 ~1.15MB/s，4.5GB≈1h）。
	Timeout time.Duration
	// AutoDelete 下载完成后是否删除网盘转存文件（节省网盘空间）。
	AutoDelete bool
	// Logger 日志。
	Logger *slog.Logger
}

// PikpakDownloader 把 PikPak 分享 URL 转存到个人网盘后用官方 CLI 完整下载。
// 实现 pkg/downloader.Downloader 接口（供 sproxy cloud download 注册表使用）。
type PikpakDownloader struct {
	cli         *Cli
	api         *API
	downloadDir string
	tempSuffix  string
	timeout     time.Duration
	autoDelete  bool
	log         *slog.Logger
}

// NewPikpakDownloader 创建下载器。
func NewPikpakDownloader(cfg DownloaderConfig) (*PikpakDownloader, error) {
	if cfg.Cli == nil {
		return nil, fmt.Errorf("pikpak downloader: cli required")
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("pikpak downloader: api required")
	}
	dir := cfg.DownloadDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "pikpak-dl")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	suffix := cfg.TempSuffix
	if suffix == "" {
		suffix = ".download"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &PikpakDownloader{
		cli: cfg.Cli, api: cfg.API, downloadDir: dir,
		tempSuffix: suffix, timeout: timeout, autoDelete: cfg.AutoDelete, log: log,
	}, nil
}

// Name 返回下载器名（注册表用）。
func (d *PikpakDownloader) Name() string { return "pikpak" }

// Supports 判断是否支持该 source（mypikpak.com/s/ 或 keepshare）。
func (d *PikpakDownloader) Supports(source string) bool {
	s := strings.ToLower(source)
	return strings.Contains(s, "mypikpak.com/s/") ||
		strings.Contains(s, "mypikpak.net/s/") ||
		strings.Contains(s, "keepshare.org/")
}

// Result 是下载完成结果（对齐 pkg/downloader.Result 语义）。
type Result = downloader.Result

// Download 下载 PikPak 分享 URL 到 destPath：
//  1. 解析分享 → 找目标文件（分享里最大的 mp4，或按文件名匹配）；
//  2. 转存到个人网盘根目录；
//  3. 网盘里定位转存文件，用官方 CLI 完整下载到 destPath；
//  4. 可选删除网盘转存（AutoDelete）。
func (d *PikpakDownloader) Download(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 1. 解析分享 ID
	shareID, err := parseShareID(source)
	if err != nil {
		return nil, err
	}
	// 2. 列出分享内容找目标文件（最大 mp4 = 全长主视频）
	files, err := d.api.ListShareRecursive(ctx, shareID)
	if err != nil {
		return nil, fmt.Errorf("pikpak list share %s: %w", shareID, err)
	}
	target := pickLargestVideo(files)
	if target == nil {
		return nil, fmt.Errorf("%w: no video in share %s", ErrShareNotFound, shareID)
	}
	d.log.Info("pikpak share target", "name", target.Name, "size", target.Size)

	// 3. 转存到个人网盘根目录
	if _, rerr := d.api.RestoreShare(ctx, shareID, []string{target.ID}, ""); rerr != nil {
		return nil, fmt.Errorf("pikpak restore %s: %w", shareID, rerr)
	}
	// 4. 网盘里定位（转存可能异步，轮询）
	var driveFile *FileMeta
	for range 10 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		driveFile, err = d.api.FindInDrive(ctx, target.Name, target.Size)
		if err == nil {
			break
		}
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
		time.Sleep(2 * time.Second)
	}
	if driveFile == nil {
		return nil, fmt.Errorf("%w: restore %s did not appear in drive", ErrFileNotFound, target.Name)
	}
	d.log.Info("pikpak drive file ready", "id", driveFile.ID, "name", driveFile.Name)

	// 5. 用官方 CLI 下载（完整，无分享 40-50% 限制）
	size, err := d.downloadViaCLI(ctx, driveFile.ID, destPath, onProgress)
	if err != nil {
		return nil, err
	}
	// 6. 可选删除网盘转存
	if d.autoDelete {
		if err := d.api.Delete(ctx, []string{driveFile.ID}); err != nil {
			d.log.Warn("pikpak auto-delete failed", "err", err, "id", driveFile.ID)
		}
	}
	return &Result{Size: size, Checksum: "", ModTime: time.Now()}, nil
}

// downloadViaCLI 用官方 CLI 下载网盘文件到 destPath。
// destPath 为空时落到 DownloadDir。
func (d *PikpakDownloader) downloadViaCLI(ctx context.Context, fileID, destPath string, onProgress func(int64, int64)) (int64, error) {
	out := destPath
	if out == "" {
		out = filepath.Join(d.downloadDir, fileID+".mp4")
	}
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	args := []string{"download", fileID, "-o", out}
	// #nosec G204 -- CLI 路径来自配置（binary_path/PATH），args 为固定子命令参数，非用户输入
	cmd := exec.CommandContext(ctx, d.cli.bin, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr // CLI 进度打到 stderr，避免污染 stdout
	cmd.Stderr = os.Stderr
	if onProgress != nil {
		// 进度：CLI 无结构化输出；改为轮询本地文件大小
		go d.pollFileSize(ctx, out, onProgress)
	}
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("pikpak cli download %s: %w", fileID, err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return 0, fmt.Errorf("pikpak cli output missing: %w", err)
	}
	return fi.Size(), nil
}

// pollFileSize 轮询本地文件大小上报进度。
func (d *PikpakDownloader) pollFileSize(ctx context.Context, path string, onProgress func(int64, int64)) {
	last := int64(0)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			sz := fi.Size()
			if sz != last {
				onProgress(sz, -1)
				last = sz
			}
		}
	}
}

// pickLargestVideo 从分享文件里挑最大的视频（全长主视频）。
func pickLargestVideo(files []FileMeta) *FileMeta {
	var best *FileMeta
	for i := range files {
		f := &files[i]
		if f.Kind != "drive#file" {
			continue
		}
		if !isVideo(f.Name, f.MimeType) {
			continue
		}
		if best == nil || f.Size > best.Size {
			best = f
		}
	}
	return best
}

// isVideo 判断是否为视频文件。
func isVideo(name, mime string) bool {
	if strings.HasPrefix(mime, "video/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".mov", ".avi", ".ts", ".flv", ".wmv", ".webm":
		return true
	}
	return false
}

// runDownloadCmd 是 exec.CommandContext 的测试替身（包级可注入）。
var runDownloadCmd = exec.CommandContext
