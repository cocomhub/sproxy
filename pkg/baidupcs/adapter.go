// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
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
