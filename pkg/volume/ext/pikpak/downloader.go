// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
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
	// 仅 owner 可写：下载中转目录不含共享需求（S5445 收紧，避免公开可写路径）。
	if err := os.MkdirAll(dir, 0o700); err != nil {
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

// Supports 判断是否支持该 source（mypikpak.com/s/、mypikpak.net/s/ 或 keepshare）。
// 与 parseShareID 共享域名判据（supportedShareHost），避免子串误匹配 query 里
// "提及"域名的普通链接被路由进 PikPak（下载会因解析失败而挂掉）。
func (d *PikpakDownloader) Supports(source string) bool {
	u, err := url.Parse(source)
	if err != nil {
		return false
	}
	if !supportedShareHost(strings.ToLower(u.Hostname())) {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	return len(parts) >= 2 && parts[0] == "s"
}

// Result 是下载完成结果（对齐 pkg/downloader.Result 语义）。
type Result = downloader.Result

// Download 下载 PikPak 分享 URL 到 destPath：
//  1. 解析分享 → 找目标文件（分享里最大的 mp4，或按文件名匹配）；
//  2. 转存到个人网盘根目录；
//  3. 网盘里定位转存文件，用官方 CLI 完整下载到 destPath；
//  4. 可选删除网盘转存（AutoDelete）。
func (d *PikpakDownloader) Download(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc) (*Result, error) {
	return d.download(ctx, source, destPath, onProgress, nil)
}

// DownloadWithWriter 实现 downloader.WriterDownloader：CLI 下载字节经 QuotaSink 记账
// （边写边记 + 配额拦截），与内置 HTTP 下载器的配额语义一致。sinkFactory 为 nil 时
// 与 Download 等价（直写 destPath）。
func (d *PikpakDownloader) DownloadWithWriter(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
	return d.download(ctx, source, destPath, onProgress, sinkFactory)
}

func (d *PikpakDownloader) download(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
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
	fileID, rerr := d.api.RestoreShare(ctx, shareID, []string{target.ID}, "")
	if rerr != nil {
		return nil, fmt.Errorf("pikpak restore %s: %w", shareID, rerr)
	}
	// 4. 网盘里定位转存文件（restore 返回的精确 fileID 优先，异步转存回退轮询）
	driveFile, err := d.locateRestoredFile(ctx, fileID, target)
	if err != nil {
		return nil, err
	}

	// 5. 用官方 CLI 下载（完整，无分享 40-50% 限制），字节经 sink 记账（若装配）
	size, checksum, err := d.downloadViaCLI(ctx, driveFile.ID, destPath, onProgress, sinkFactory)
	if err != nil {
		return nil, err
	}
	// 6. 可选删除网盘转存（仅删除精确命中的转存文件，杜绝误删网盘旧文件）
	if d.autoDelete {
		if err := d.api.Delete(ctx, []string{driveFile.ID}); err != nil {
			d.log.Warn("pikpak auto-delete failed", "err", err, "id", driveFile.ID)
		}
	}
	return &Result{Size: size, Checksum: checksum, ModTime: time.Now()}, nil
}

// locateRestoredFile 定位转存后的网盘文件：restore 返回的精确 fileID 优先；
// 转存可能异步（RESTORE_START 时 fileID 为占位/未知），且同尺寸/同子串的旧文件
// 可能被模糊匹配误命中——故未命中精确 ID 才回退「按名字+大小」轮询。
func (d *PikpakDownloader) locateRestoredFile(ctx context.Context, fileID string, target *FileMeta) (*FileMeta, error) {
	if fileID != "" {
		driveFile, err := d.api.FindByID(ctx, fileID)
		if err != nil && !errors.Is(err, ErrFileNotFound) {
			return nil, fmt.Errorf("pikpak find restored file %s: %w", fileID, err)
		}
		if driveFile != nil {
			d.log.Info("pikpak drive file ready (exact id)", "id", driveFile.ID, "name", driveFile.Name)
			return driveFile, nil
		}
	}
	// 回退：轮询按名字+大小查找（转存完成前可能短暂不可见）。
	for range 10 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		driveFile, err := d.api.FindInDrive(ctx, target.Name, target.Size)
		if err == nil {
			d.log.Info("pikpak drive file ready", "id", driveFile.ID, "name", driveFile.Name)
			return driveFile, nil
		}
		if err != nil && !errors.Is(err, ErrFileNotFound) {
			return nil, err
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("%w: restore %s did not appear in drive", ErrFileNotFound, target.Name)
}

// downloadViaCLI 用官方 CLI 下载网盘文件到 destPath，返回字节数与 SHA-256。
// sinkFactory 非空时，写盘字节经 QuotaSink 记账（边写边记 + 配额拦截），否则直写。
// 超时：以 d.timeout 为 CLI 执行硬 deadline（构造时已设默认 2h）；pollFileSize
// 进度轮询共用该 ctx，CLI 结束/超时即随 ctx 一并停止（不泄漏 goroutine）。
func (d *PikpakDownloader) downloadViaCLI(ctx context.Context, fileID, destPath string, onProgress func(int64, int64), sinkFactory downloader.SinkFactory) (int64, string, error) {
	cliCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	out := destPath
	if out == "" {
		out = filepath.Join(d.downloadDir, fileID+".mp4")
	}
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", err
	}
	args := []string{"download", fileID, "-o", out}
	// #nosec G204 -- CLI 路径来自配置（binary_path/PATH），args 为固定子命令参数，非用户输入
	cmd := exec.CommandContext(cliCtx, d.cli.bin, args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr // CLI 进度打到 stderr，避免污染 stdout
	cmd.Stderr = os.Stderr
	if onProgress != nil {
		// 进度：CLI 无结构化输出；改为轮询本地文件大小（随 cliCtx 退出，无泄漏）。
		go d.pollFileSize(cliCtx, out, onProgress)
	}
	if err := cmd.Run(); err != nil {
		return 0, "", fmt.Errorf("pikpak cli download %s: %w", fileID, err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return 0, "", fmt.Errorf("pikpak cli output missing: %w", err)
	}
	// 字节经 sink 记账（若装配）：把已落盘文件流式读回，经 sink 写盘边写边记。
	// 说明：CLI 已直写 destPath，这里以「文件 → sink → destPath」重放一遍完成记账；
	// 等同内置 HTTP 下载器经 QuotaWriter 的配额语义（sink 的 Write 累加 committed）。
	if sinkFactory != nil {
		sink, sinkErr := sinkFactory(newDeferredSinkWriter(out), fi.Size(), false)
		if sinkErr != nil {
			return 0, "", sinkErr
		}
		if replayErr := replayFileIntoSink(out, sink); replayErr != nil {
			sink.Finish(false, 0)
			return 0, "", replayErr
		}
		sink.Finish(true, 0)
	}
	checksum, err := sha256File(out)
	if err != nil {
		return 0, "", err
	}
	return fi.Size(), checksum, nil
}

// replayFileIntoSink 把已落盘文件流式重放进 sink（配额记账）。
func replayFileIntoSink(path string, sink downloader.QuotaSink) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 128*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := sink.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// sha256File 计算文件的 SHA-256 十六进制。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
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

// deferredSinkWriter 是占位写盘目标：sink 记账用（Write 实际丢弃，字节已由 CLI 直写
// destPath，此处仅作 committed 累加）。
type deferredSinkWriter struct{}

func (w deferredSinkWriter) Write(p []byte) (int, error) { return len(p), nil }

func newDeferredSinkWriter(_ string) deferredSinkWriter { return deferredSinkWriter{} }

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
