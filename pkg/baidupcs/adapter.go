// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Adapter 是百度网盘底层执行器接口（二进制优先 + 库兜底双路径的抽象）。
type Adapter interface {
	Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error
	Download(ctx context.Context, remotePath, localPath string) error
}

// libraryFallback 是库兜底的最小接口（真实库 adapter 与测试 fake 都实现）。
type libraryFallback interface {
	Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error
	Download(ctx context.Context, remotePath, localPath string) error
}

// AdapterConfig 是 binaryAdapter 配置。
type AdapterConfig struct {
	// BinaryPath 是 BaiduPCS-Go 可执行文件路径；空 = PATH 查找。
	BinaryPath string
	// BinaryTimeout 是子进程执行超时；0 = 默认 10 分钟（大文件传输）。
	BinaryTimeout time.Duration
	// Logger 是日志（nil = 静默）。
	Logger *slog.Logger
	// Fallback 是库兜底实现（nil = 无兜底，二进制失败直接报错）。
	Fallback libraryFallback
}

// defaultBinaryTimeout 是子进程默认超时（10 分钟，覆盖大文件传输）。
const defaultBinaryTimeout = 10 * time.Minute

// binaryAdapter 是二进制优先执行器：exec BaiduPCS-Go 命令，失败/缺失/超时回退库。
type binaryAdapter struct {
	cfg AdapterConfig
}

// newBinaryAdapter 创建二进制优先执行器。
func newBinaryAdapter(cfg AdapterConfig) *binaryAdapter {
	if cfg.BinaryTimeout <= 0 {
		cfg.BinaryTimeout = defaultBinaryTimeout
	}
	return &binaryAdapter{cfg: cfg}
}

func (a *binaryAdapter) logger() *slog.Logger {
	if a.cfg.Logger != nil {
		return a.cfg.Logger
	}
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

// runBinary 执行 BaiduPCS-Go 子命令。返回 (成功与否, 错误)。
// 二进制不存在 / 超时 / 非零退出均视为失败 → 调用方回退库。
func (a *binaryAdapter) runBinary(ctx context.Context, args ...string) (bool, error) {
	bin := a.cfg.BinaryPath
	if bin == "" {
		bin = "BaiduPCS-Go"
	}
	timeout := a.cfg.BinaryTimeout
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		a.logger().Info("baidupcs 二进制执行失败，回退库",
			"bin", bin, "args", args, "err", err, "output", truncate(string(out), 200))
		return false, fmt.Errorf("BaiduPCS-Go %v: %w", args, err)
	}
	return true, nil
}

// Upload 二进制优先上传，失败（缺失/超时/非零退出）回退库。
func (a *binaryAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	policy := "skip"
	if overwrite {
		policy = "overwrite"
	}
	ok, err := a.runBinary(ctx, "upload", "--policy", policy, localPath, targetPath)
	if ok {
		return nil
	}
	// 失败（含超时/二进制缺失）：有兜底则回退，无兜底才返回错误。
	if a.cfg.Fallback != nil {
		fbErr := a.cfg.Fallback.Upload(ctx, localPath, targetPath, overwrite)
		if fbErr != nil {
			return fmt.Errorf("baidupcs upload: 二进制失败(%v) 且库兜底也失败: %w", err, fbErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("baidupcs upload 失败且无库兜底")
}

// Download 二进制优先下载，失败（缺失/超时/非零退出）回退库。
func (a *binaryAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	ok, err := a.runBinary(ctx, "download", "--saveto", localPath, remotePath)
	if ok {
		return nil
	}
	if a.cfg.Fallback != nil {
		fbErr := a.cfg.Fallback.Download(ctx, remotePath, localPath)
		if fbErr != nil {
			return fmt.Errorf("baidupcs download: 二进制失败(%v) 且库兜底也失败: %w", err, fbErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("baidupcs download 失败且无库兜底")
}

// discard 是 io.Writer 的静默实现（slog 无日志时用）。
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// libraryAdapter 是库兜底实现（分片上传 MultiUploader + 下载断点恢复）。
// 二进制缺失/失败/超时时由 binaryAdapter 的 Fallback 调用；脱离二进制完整可用。
type libraryAdapter struct {
	pcs    *Client
	log    *slog.Logger
	layout *Layout // 断点/暂存布局；nil = 断点不持久化
}

// newLibraryAdapter 创建库兜底 adapter。
func newLibraryAdapter(pcs *Client, logger *slog.Logger) *libraryAdapter {
	return &libraryAdapter{pcs: pcs, log: logger, layout: globalLayout}
}

// Upload 用 fork 库的分片上传器（NewMultiUploader）上传本地文件到网盘。
// 走 Precreate→分片 TmpFile→CreateSuperFile；断点状态持久化到 Layout.Resume。
// 这是 P2 真实现——脱离二进制也完整可用（二进制优先策略下是可靠兜底）。
func (a *libraryAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	if a.pcs == nil {
		return fmt.Errorf("baidupcs: library adapter without client")
	}
	a.log.Info("baidupcs 库兜底 Upload（分片上传器）", "local", localPath, "target", targetPath)
	resumeKey := ""
	if a.layout != nil {
		resumeKey = a.layout.SanitizeKey(targetPath) + ":" + sanitizeRemotePathSize(localPath)
	}
	return uploadViaMultiUploader(ctx, a.pcs, localPath, targetPath, overwrite, resumeKey)
}

// Download 用 fork 库的 Downloader（Range 并行 + 断点恢复）下载网盘文件到本地。
// 断点文件在 <Layout.Tmp>/<key>.download（JSON），完成时删除。
// 这是 P2 真实现——脱离二进制也完整可用。
func (a *libraryAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	if a.pcs == nil {
		return fmt.Errorf("baidupcs: library adapter without client")
	}
	layout := a.layout
	if layout == nil {
		l, lErr := NewLayout(filepath.Join(os.TempDir(), "baidupcs"))
		if lErr != nil {
			return fmt.Errorf("baidupcs: init layout: %w", lErr)
		}
		layout = l
	}
	a.log.Info("baidupcs 库兜底 Download（Downloader + 断点）", "remote", remotePath, "local", localPath)
	return downloadViaDownloader(ctx, a.pcs, remotePath, localPath, layout)
}
