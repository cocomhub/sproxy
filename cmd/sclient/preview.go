// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
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

func previewText(serverURL, authToken, filename string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		serverURL+"/download?filename="+filename, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载文件失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载文件失败: HTTP %d", resp.StatusCode)
	}

	// 只读取前 64KB 并显示前 100 行
	limitedReader := io.LimitReader(resp.Body, 64*1024)
	var buf bytes.Buffer
	teeReader := io.TeeReader(limitedReader, &buf)

	scanner := bufio.NewScanner(teeReader)
	maxLines := 100
	lineCount := 0

	fmt.Printf("--- 文件预览: %s ---\n", filename)
	for scanner.Scan() && lineCount < maxLines {
		fmt.Println(scanner.Text())
		lineCount++
	}

	// 检查是否还有更多内容
	hasMore := false
	for scanner.Scan() {
		hasMore = true
		break
	}

	if hasMore || lineCount >= maxLines {
		fmt.Printf("\n... (仅显示前 %d 行)\n", maxLines)
	}

	return nil
}

func previewImage(serverURL, authToken, filename string) error {
	tmpDir, err := os.MkdirTemp("", "sproxy-preview-*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpFile := filepath.Join(tmpDir, filepath.Base(filename))

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		serverURL+"/download?filename="+filename, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载文件失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载文件失败: HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	fmt.Printf("正在打开图片预览: %s\n", tmpFile)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", tmpFile)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", tmpFile)
	default:
		cmd = exec.Command("xdg-open", tmpFile)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("打开图片查看器失败: %w", err)
	}

	fmt.Println("图片查看器已打开，按 Enter 键清理临时文件...")
	fmt.Scanln()

	return nil
}

// NewCmdPreview 创建 preview 命令的工厂函数版本。
// preview 命令不使用 client.Service 接口，而是直接使用 http.DefaultClient
// 通过 /download 端点获取文件内容进行预览。
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

			serverURL, authToken := getCloudServerURL(cmd)
			if serverURL == "" {
				return fmt.Errorf("未指定服务器地址，请使用 --server 或配置 server_url")
			}

			ext := strings.ToLower(filepath.Ext(filename))
			if isImageExt(ext) {
				return previewImage(serverURL, authToken, filename)
			}
			if isTextExt(ext) {
				return previewText(serverURL, authToken, filename)
			}
			return fmt.Errorf("无法预览此文件类型: %s", ext)
		},
	}
	return cmd
}
