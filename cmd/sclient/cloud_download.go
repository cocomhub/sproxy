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
	Use:   "cloud-download <url>",
	Short: "从云端下载文件（服务端先拉取，再下载到本地）",
	Long: `通过 sproxy 服务端从外部 URL 下载文件，完成后自动下载到本地并清理云端副本。

小文件（< 20 MiB）默认同步等待，大文件自动切换异步模式。
如果同步下载过程中连接断开，服务端自动转为异步模式继续下载。`,
	Args: cobra.ExactArgs(1),
	RunE: runCloudDownload,
}

func init() {
	cloudDownloadCmd.Flags().Bool("force-async", false, "强制使用异步模式（即使文件小于阈值）")
	cloudDownloadCmd.Flags().Bool("no-cleanup", false, "下载到本地后不删除云端副本")
	cloudDownloadCmd.Flags().Duration("poll-interval", 2*time.Second, "异步模式轮询间隔")
}

func runCloudDownload(cmd *cobra.Command, args []string) error {
	urlStr := args[0]
	forceAsync, _ := cmd.Flags().GetBool("force-async")
	noCleanup, _ := cmd.Flags().GetBool("no-cleanup")
	pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
	outputPath, _ := cmd.Flags().GetString("output")

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

	// 1. 创建云端下载任务
	task, err := createCloudDownloadTask(serverURL, urlStr, forceAsync)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建云端下载任务失败: %v\n", err)
		return fmt.Errorf("创建云端下载任务失败: %w", err)
	}

	fmt.Printf("任务 ID: %s\n", task.ID)
	fmt.Printf("状态: %s\n", task.Status)

	// 2. 如果同步模式完成，直接进入下载
	if task.Status == "completed" {
		return downloadAndCleanup(serverURL, task, outputPath, noCleanup)
	}

	// 3. 异步模式：轮询等待完成
	if forceAsync {
		fmt.Println("强制异步模式，轮询任务状态...")
	} else {
		fmt.Println("异步模式，轮询任务状态...")
	}

	task, err = pollCloudTask(serverURL, task.ID, pollInterval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "云端下载任务失败: %v\n", err)
		return fmt.Errorf("云端下载任务失败: %w", err)
	}

	// 4. 下载到本地并清理云端
	return downloadAndCleanup(serverURL, task, outputPath, noCleanup)
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

func createCloudDownloadTask(serverURL, sourceURL string, forceAsync bool) (*cloudTaskResponse, error) {
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

func pollCloudTask(serverURL, taskID string, interval time.Duration) (*cloudTaskResponse, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		req, err := http.NewRequest(http.MethodGet, serverURL+"/api/cloud/tasks/"+taskID, nil)
		if err != nil {
			return nil, err
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

func downloadAndCleanup(serverURL string, task *cloudTaskResponse, outputPath string, noCleanup bool) error {
	// 确定输出路径：默认仅使用文件名，拒绝路径穿越
	if outputPath == "" {
		outputPath = filepathSafe(filepath.Base(task.Filename))
	}

	// 1. 下载文件：URL 编码路径参数，防止路径注入
	downloadURL := fmt.Sprintf("%s/download?filename=%s",
		serverURL, url.QueryEscape(".__cloud__/"+task.ID+"/"+task.Filename))
	resp, err := http.Get(downloadURL)
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
