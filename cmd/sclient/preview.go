// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".svg":
		return true
	}
	return false
}

func isTextExt(ext string) bool {
	switch ext {
	case ".txt", ".md", ".json", ".yaml", ".yml", ".log", ".csv",
		".go", ".py", ".js", ".ts", ".html", ".css", ".xml", ".sh",
		".bat", ".ps1", ".toml", ".ini", ".cfg", ".conf", ".env",
		".gitignore", ".dockerfile", ".makefile", ".sql", ".rb", ".java", ".rs":
		return true
	}
	return false
}

// previewText 经 SDK 流式下载并只显示前 100 行（最多读 64KB）。
//
// 走 `FileClient.OpenDownload` 而非自建 HTTP：签名（SproxySig）/隧道/直连、TLS 与 CA、
// 错误映射全部由 SDK 承担。此前 cmd 内自行 `http.NewRequest` + `sproxysig.Sign…`，
// 既重复实现又**不支持隧道模式**（同一份逻辑在 SDK 与 cmd 各维护一遍）。
func previewText(ctx context.Context, ios cli.IOStreams, svc *client.FileClient, filename string) error {
	rc, err := svc.OpenDownload(ctx, filename)
	if err != nil {
		return fmt.Errorf("下载文件失败: %w", err)
	}
	defer func() { _ = rc.Close() }()

	fmt.Fprintf(ios.Out, "--- 文件预览: %s ---\n", filename)

	const (
		previewMaxBytes = 64 * 1024
		previewMaxLines = 100
	)
	scanner := bufio.NewScanner(io.LimitReader(rc, previewMaxBytes))
	lineCount := 0
	for scanner.Scan() && lineCount < previewMaxLines {
		fmt.Fprintln(ios.Out, scanner.Text())
		lineCount++
	}
	// 达到行数上限，或仍有未显示的后续行 ⇒ 提示截断。
	truncated := lineCount >= previewMaxLines
	if !truncated && scanner.Scan() {
		truncated = true
	}
	if truncated {
		fmt.Fprintf(ios.Out, "\n... (仅显示前 %d 行)\n", previewMaxLines)
	}
	return nil
}

// openViewer 使用系统默认程序打开文件。可在测试中替换，避免依赖系统行为。
var openViewer = func(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

// previewImage 经 SDK 下载到临时文件并用系统图片查看器打开。
func previewImage(ctx context.Context, ios cli.IOStreams, svc *client.FileClient, filename string) error {
	tmpDir, err := os.MkdirTemp("", "sproxy-preview-*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpFile := filepath.Join(tmpDir, filepath.Base(filename))

	rc, err := svc.OpenDownload(ctx, filename)
	if err != nil {
		return fmt.Errorf("下载文件失败: %w", err)
	}
	defer func() { _ = rc.Close() }()

	out, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		return fmt.Errorf("写入文件失败: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("关闭文件失败: %w", err)
	}

	fmt.Fprintf(ios.Out, "正在打开图片预览: %s\n", tmpFile)

	if err := openViewer(tmpFile); err != nil {
		return fmt.Errorf("打开图片查看器失败: %w", err)
	}

	fmt.Fprintln(ios.Out, "图片查看器已打开，按 Enter 键清理临时文件（5 秒后自动清理）...")
	done := make(chan struct{})
	go func() {
		_, _ = fmt.Fscanln(ios.In)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		fmt.Fprintln(ios.Out, "超时，自动清理临时文件")
	}

	return nil
}

// NewCmdPreview 创建 preview 命令的工厂函数版本。
//
// 文件内容经 `FileClient` 获取（`OpenDownload`）：服务端地址、SproxySig 凭据、隧道/直连
// 选路与 TLS 全部由工厂构造的客户端承担，命令本身只做扩展名分派与展示。
func NewCmdPreview(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview <filename>",
		Short: "预览服务端文件",
		Long: `预览服务端上的文件内容。

		文本文件（.txt, .md, .json, .yaml, .log, .csv, .go, .py, .js 等）：
		下载前 100 行输出到终端。

		图片文件（.png, .jpg, .jpeg, .gif, .bmp, .webp, .svg）：
		下载到临时目录并使用系统图片查看器打开。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			ext := strings.ToLower(filepath.Ext(filename))
			switch {
			case isImageExt(ext):
				return previewImage(cmd.Context(), ios, svc, filename)
			case isTextExt(ext):
				return previewText(cmd.Context(), ios, svc, filename)
			}
			return fmt.Errorf("无法预览此文件类型: %s", ext)
		},
	}
	return cmd
}
