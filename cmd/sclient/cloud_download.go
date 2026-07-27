// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// readURLsFromFile 从文件中读取 URL 列表（每行一个）。
// 忽略空行和 # 开头的注释行，去除每行首尾空白。
func readURLsFromFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var urls []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, nil
}

// NewCmdCloudDownload 创建云端下载命令的工厂函数。
func NewCmdCloudDownload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-download <url> [url...]",
		Short: "从云端下载文件（服务端先拉取，再下载到本地）",
		Long: `通过 sproxy 服务端从外部 URL 下载文件，完成后自动下载到本地并清理云端副本。

小文件（< 20 MiB）默认同步等待，大文件自动切换异步模式。
如果同步下载过程中连接断开，服务端自动转为异步模式继续下载。

支持多个 URL 参数或通过 --batch 从文件读取 URL 列表。

使用 --wait 可等待所有任务完成后自动进入归档/下载/解压链式操作。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			wait, _ := cmd.Flags().GetBool("wait")
			archive, _ := cmd.Flags().GetBool("archive")
			download, _ := cmd.Flags().GetBool("download")
			extract, _ := cmd.Flags().GetBool("extract")
			archiveName, _ := cmd.Flags().GetString("archive-name")
			outputDir, _ := cmd.Flags().GetString("output-dir")
			noCleanup, _ := cmd.Flags().GetBool("no-cleanup")
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			batchFile, _ := cmd.Flags().GetString("batch")

			// 收集 URL
			urls := args
			if batchFile != "" {
				var fileURLs []string
				fileURLs, err = readURLsFromFile(batchFile)
				if err != nil {
					return fmt.Errorf("读取 batch 文件失败: %w", err)
				}
				urls = append(urls, fileURLs...)
			}
			if len(urls) == 0 {
				return fmt.Errorf("未指定下载 URL，请提供 URL 参数或使用 --batch 指定文件")
			}

			// 创建任务
			ios.WriteOutLine("创建云端下载任务...")
			tasks, err := svc.CloudDownloadBatch(cmd.Context(), urls)
			if err != nil {
				return fmt.Errorf("创建云端下载任务失败: %w", err)
			}

			// 输出任务信息
			for _, t := range tasks {
				statusLine := fmt.Sprintf("  %s: %s", t.ID, t.Status)
				if t.Filename != "" {
					statusLine += fmt.Sprintf(" (%s)", t.Filename)
				}
				if t.Error != "" {
					statusLine += fmt.Sprintf(" - %s", t.Error)
				}
				ios.WriteOutLine(statusLine)
			}

			// 等待模式
			if wait {
				completedTasks, waitErr := waitForCompletion(cmd.Context(), svc, ios, tasks, pollInterval)
				if waitErr != nil {
					return waitErr
				}

				// 收集成功任务
				var succeededIDs []string
				var succeededTask *client.CloudTask
				for _, t := range completedTasks {
					if t.Status == "completed" {
						succeededIDs = append(succeededIDs, t.ID)
						if succeededTask == nil {
							succeededTask = &t
						}
					}
				}
				if len(succeededIDs) == 0 {
					return fmt.Errorf("所有任务均未成功完成")
				}

				// 归档模式
				if archive {
					name := archiveName
					if name == "" {
						if len(succeededIDs) == 1 && succeededTask != nil {
							name = succeededTask.Filename
						} else {
							name = fmt.Sprintf("cloud-batch-%d", time.Now().Unix())
						}
					}
					ios.WriteOutLine("打包归档中...")
					var result *client.ArchiveResult
					if len(succeededIDs) == 1 {
						result, err = svc.ArchiveCloudTask(cmd.Context(), succeededIDs[0], name)
					} else {
						result, err = svc.ArchiveCloudTasks(cmd.Context(), succeededIDs, name)
					}
					if err != nil {
						return fmt.Errorf("归档失败: %w", err)
					}
					ios.WriteOutLine("  归档完成: %s (%d bytes)", result.File, result.Size)

					// 下载模式
					if download {
						outputPath := filepath.Join(outputDir, result.File)
						ios.WriteOutLine("下载归档到本地: %s", outputPath)
						if err := svc.Download(cmd.Context(), result.File, outputPath); err != nil {
							return fmt.Errorf("下载归档失败: %w", err)
						}
						ios.WriteOutLine("  下载完成")

						// 解压模式
						if extract {
							ios.WriteOutLine("解压中...")
							if err := extractTarGz(outputPath, outputDir); err != nil {
								return fmt.Errorf("解压失败: %w", err)
							}
							ios.WriteOutLine("  解压完成: %s", outputDir)
						}
					}
				}

				// 清理云端任务
				if !noCleanup {
					for _, id := range succeededIDs {
						_ = svc.DeleteCloudTask(cmd.Context(), id)
					}
				}
			}
			return nil
		},
	}

	// 注册 flags
	cmd.Flags().Bool("no-cleanup", false, "下载到本地后不删除云端副本")
	cmd.Flags().Duration("poll-interval", 2*time.Second, "异步模式轮询间隔")
	cmd.Flags().String("batch", "", "从文件读取 URL 列表（每行一个 URL，忽略空行和 # 注释行）")
	cmd.Flags().Bool("wait", false, "等待所有任务完成后自动进入归档/下载/解压链式操作")
	cmd.Flags().Bool("archive", false, "完成后打包归档")
	cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
	cmd.Flags().Bool("download", false, "下载归档到本地")
	cmd.Flags().String("output-dir", ".", "本地输出目录")
	cmd.Flags().Bool("extract", false, "解压归档（仅与 --download 同时使用）")

	// 注册子命令
	cmd.AddCommand(NewCmdCloudList(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudCancel(factory, ios, cfgSvc))

	return cmd
}

// waitForCompletion 轮询等待所有云端任务完成。
func waitForCompletion(ctx context.Context, fc *client.FileClient, ios cli.IOStreams, tasks []client.CloudTask, interval time.Duration) ([]client.CloudTask, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}

	pending := make(map[string]client.CloudTask)
	for _, t := range tasks {
		if t.Status == "pending" || t.Status == "downloading" {
			pending[t.ID] = t
		}
	}

	if len(pending) == 0 {
		return tasks, nil
	}

	ios.WriteOutLine("等待 %d 个任务完成...", len(pending))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	results := make([]client.CloudTask, 0, len(tasks))
	for _, t := range tasks {
		if t.Status == "pending" || t.Status == "downloading" {
			continue
		}
		results = append(results, t)
	}

	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			for id := range pending {
				task, err := fc.GetCloudTask(ctx, id)
				if err != nil {
					ios.WriteErrLine("  轮询任务 %s 失败: %v", id, err)
					delete(pending, id)
					continue
				}
				switch task.Status {
				case "completed":
					delete(pending, id)
					results = append(results, *task)
					ios.WriteOutLine("  ✓ %s: 完成 (%s, %d bytes)", id, task.Filename, task.TotalSize)
				case "failed":
					delete(pending, id)
					results = append(results, *task)
					ios.WriteErrLine("  ✗ %s: 失败 - %s", id, task.Error)
				case "cancelled":
					delete(pending, id)
					results = append(results, *task)
					ios.WriteErrLine("  ✗ %s: 已取消", id)
				default:
					pct := int64(0)
					if task.TotalSize > 0 {
						pct = task.Downloaded * 100 / task.TotalSize
					}
					ios.WriteOutLine("  ⟳ %s: %d%% (%d/%d bytes)", id, pct, task.Downloaded, task.TotalSize)
				}
			}
		}
	}
	return results, nil
}

// extractTarGz 解压 tar.gz 文件到指定目录。
func extractTarGz(src, destDir string) error {
	file, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开归档文件失败: %w", err)
	}
	defer file.Close()

	gr, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("创建 gzip reader 失败: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 头失败: %w", err)
		}

		targetPath := filepath.Join(destDir, filepath.Clean(header.Name))
		if !strings.HasPrefix(targetPath, filepath.Clean(destDir)+string(filepath.Separator)) {
			// 路径穿越保护
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("创建目录失败: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("创建目录失败: %w", err)
			}
			outFile, err := os.Create(targetPath)
			if err != nil {
				return fmt.Errorf("创建文件失败: %w", err)
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return fmt.Errorf("写入文件失败: %w", err)
			}
			outFile.Close()
		}
	}
	return nil
}
