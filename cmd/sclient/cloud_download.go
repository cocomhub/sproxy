// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var cloudDownloadCmd = &cobra.Command{
	Use:   "cloud-download <url> [url...]",
	Short: "从云端下载文件（服务端先拉取，再下载到本地）",
	Long: `通过 sproxy 服务端从外部 URL 下载文件，完成后自动下载到本地并清理云端副本。

小文件（< 20 MiB）默认同步等待，大文件自动切换异步模式。
如果同步下载过程中连接断开，服务端自动转为异步模式继续下载。

支持多个 URL 参数或通过 --batch 从文件读取 URL 列表。`,
	Args: cobra.ArbitraryArgs,
	RunE: runCloudDownload,
}

func init() {
	cloudDownloadCmd.Flags().Bool("force-async", false, "强制使用异步模式（即使文件小于阈值）")
	cloudDownloadCmd.Flags().Bool("no-cleanup", false, "下载到本地后不删除云端副本")
	cloudDownloadCmd.Flags().Duration("poll-interval", 2*time.Second, "异步模式轮询间隔")
	cloudDownloadCmd.Flags().String("batch", "", "从文件读取 URL 列表（每行一个 URL，忽略空行和 # 注释行）")
}

func runCloudDownload(cmd *cobra.Command, args []string) error {
	forceAsync, _ := cmd.Flags().GetBool("force-async")
	noCleanup, _ := cmd.Flags().GetBool("no-cleanup")
	pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
	outputPath, _ := cmd.Flags().GetString("output")
	batchFile, _ := cmd.Flags().GetString("batch")

	// 获取 auth token: 优先 --auth-token flag，其次配置文件
	authToken, _ := cmd.Flags().GetString("auth-token")
	if authToken == "" && cfgProvider != nil {
		cfg, err := client.LoadFromProvider(cfgProvider)
		if err == nil {
			authToken = cfg.AuthToken
		}
	}

	// 收集所有 URL
	urls := args
	if batchFile != "" {
		fileURLs, err := readURLsFromFile(batchFile)
		if err != nil {
			return fmt.Errorf("读取 batch 文件失败: %w", err)
		}
		urls = append(urls, fileURLs...)
	}
	if len(urls) == 0 {
		return fmt.Errorf("未指定下载 URL，请提供 URL 参数或使用 --batch 指定文件")
	}
	if len(urls) > 1 && outputPath != "" {
		return fmt.Errorf("多个 URL 不支持 --output 标志，每个文件将使用其原始文件名保存")
	}

	// 获取 serverURL: 优先 flag，其次配置
	serverURL, _ := cmd.Flags().GetString("server")
	if serverURL == "" && cfgProvider != nil {
		cfg, err := client.LoadFromProvider(cfgProvider)
		if err == nil {
			serverURL = cfg.ServerURL
		}
	}
	if serverURL == "" {
		return fmt.Errorf("未指定服务器地址，请使用 --server 或配置 server_url")
	}

	succeeded := 0
	failed := 0
	for i, urlStr := range urls {
		if len(urls) > 1 {
			fmt.Printf("[%d/%d] %s\n", i+1, len(urls), urlStr)
		}

		// 1. 创建云端下载任务
		task, err := createCloudDownloadTask(serverURL, urlStr, authToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  创建云端下载任务失败: %v\n", err)
			failed++
			continue
		}

		if len(urls) == 1 {
			fmt.Printf("任务 ID: %s\n", task.ID)
			fmt.Printf("状态: %s\n", task.Status)
		}

		// 2. 如果同步模式完成，直接进入下载
		if task.Status == "completed" {
			if dlErr := downloadAndCleanup(serverURL, task, outputPath, noCleanup, authToken); dlErr != nil {
				fmt.Fprintf(os.Stderr, "  %v\n", dlErr)
				failed++
			} else {
				succeeded++
			}
			continue
		}

		// 3. 异步模式：轮询等待完成
		if forceAsync {
			fmt.Println("  强制异步模式，轮询任务状态...")
		} else {
			fmt.Println("  异步模式，轮询任务状态...")
		}

		task, err = pollCloudTask(serverURL, task.ID, pollInterval, authToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  云端下载任务失败: %v\n", err)
			failed++
			continue
		}

		// 4. 下载到本地并清理云端
		if err := downloadAndCleanup(serverURL, task, outputPath, noCleanup, authToken); err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n", err)
			failed++
		} else {
			succeeded++
		}
	}

	if len(urls) > 1 {
		fmt.Printf("\nSummary: %d/%d succeeded\n", succeeded, len(urls))
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d tasks failed", failed, len(urls))
	}
	return nil
}

// readURLsFromFile 从文件中读取 URL 列表（每行一个）。
// 忽略空行和 # 开头的注释行，去除每行首尾空白。
func readURLsFromFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, nil
}

type cloudTaskResponse struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	Filename   string `json:"filename"`
	Status     string `json:"status"`
	TotalSize  int64  `json:"total_size"`
	Downloaded int64  `json:"downloaded"`
	Checksum   string `json:"checksum"`
	Error      string `json:"error"`
}

func createCloudDownloadTask(serverURL, sourceURL, authToken string) (*cloudTaskResponse, error) {
	// 使用 json.Marshal 构建请求体，防止 JSON 注入
	body, _ := json.Marshal(struct {
		URL string `json:"url"`
	}{
		URL: sourceURL,
	})
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/cloud/download", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var task cloudTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, err
	}
	return &task, nil
}

func pollCloudTask(serverURL, taskID string, interval time.Duration, authToken string) (*cloudTaskResponse, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		req, err := http.NewRequest(http.MethodGet, serverURL+"/api/cloud/tasks/"+taskID, nil)
		if err != nil {
			return nil, err
		}
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "轮询失败: %v\n", err)
			continue
		}

		var task cloudTaskResponse
		if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		switch task.Status {
		case "completed":
			return &task, nil
		case "failed":
			return nil, fmt.Errorf("task failed: %s", task.Error)
		case "cancelled":
			return nil, fmt.Errorf("task was cancelled")
		case "downloading":
			pct := int64(0)
			if task.TotalSize > 0 {
				pct = task.Downloaded * 100 / task.TotalSize
			}
			fmt.Printf("\r下载进度: %d%% (%d/%d bytes)", pct, task.Downloaded, task.TotalSize)
		}
	}
	return nil, fmt.Errorf("polling ended unexpectedly")
}

func downloadAndCleanup(serverURL string, task *cloudTaskResponse, outputPath string, noCleanup bool, authToken string) error {
	// 确定输出路径：默认仅使用文件名，拒绝路径穿越
	if outputPath == "" {
		outputPath = filepathSafe(filepath.Base(task.Filename))
	}

	// 1. 下载文件：URL 编码路径参数，防止路径注入
	downloadURL := fmt.Sprintf("%s/download?filename=%s",
		serverURL, url.QueryEscape(".__cloud__/"+task.ID+"/"+task.Filename))
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("创建下载请求失败: %w", err)
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

	// 确保输出目录存在
	if dir := filepath.Dir(outputPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建输出目录失败: %w", err)
		}
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	tee := io.TeeReader(resp.Body, h)

	if _, err := io.Copy(f, tee); err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("写入文件失败: %w", err)
	}

	// 2. 校验 checksum
	localChecksum := hex.EncodeToString(h.Sum(nil))
	expectedChecksum := resp.Header.Get("X-File-Checksum")
	if expectedChecksum == "" {
		expectedChecksum = task.Checksum
	}

	if expectedChecksum != "" && localChecksum != expectedChecksum {
		fmt.Fprintf(os.Stderr, "checksum 不匹配: 期望 %s, 实际 %s\n", expectedChecksum, localChecksum)
		return fmt.Errorf("checksum 不匹配")
	}

	// 3. 清理云端副本
	if !noCleanup {
		cloudPath := url.QueryEscape(".__cloud__/" + task.ID + "/" + task.Filename)

		// 删除云端文件
		delReq, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/delete?filename=%s", serverURL, cloudPath), nil)
		delReq.Header.Set("X-File-Checksum", localChecksum)
		delResp, err := http.DefaultClient.Do(delReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "清理云端文件失败: %v\n", err)
		} else {
			delResp.Body.Close()
		}

		// 删除云端任务
		delTaskReq, _ := http.NewRequest(http.MethodDelete,
			fmt.Sprintf("%s/api/cloud/tasks/%s", serverURL, task.ID), nil)
		delTaskResp, err := http.DefaultClient.Do(delTaskReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "清理云端任务失败: %v\n", err)
		} else {
			delTaskResp.Body.Close()
		}
	}

	fmt.Printf("Downloaded %s (%d bytes)\n", outputPath, task.TotalSize)
	return nil
}

// filepathSafe 清理文件名中的路径分隔符和危险字符，防止路径穿越。
func filepathSafe(name string) string {
	// 替换路径分隔符
	name = strings.NewReplacer("\\", "_", "/", "_").Replace(name)
	// 去除首尾空格和点（防止隐藏文件）
	name = strings.Trim(name, " .")
	if name == "" {
		return "download"
	}
	return name
}
