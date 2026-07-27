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
// 默认行为是完整链式操作：提交 → 等待 → 打包 → 下载 → 清理。
func NewCmdCloudDownload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-download <url> [url...]",
		Short: "从云端下载文件（链式操作：提交→等待→打包→下载→清理）",
		Long: `通过 sproxy 服务端从外部 URL 下载文件，自动执行完整链式操作：

  1. 提交云端下载任务
  2. 等待下载完成
  3. 打包归档为 tar.gz
  4. 下载到本地
  5. 清理远端文件

使用 --keep-files 跳过清理步骤。
使用子命令（submit, wait, archive, fetch, list, cancel, resume）执行单个步骤。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			archiveName, _ := cmd.Flags().GetString("archive-name")
			outputDir, _ := cmd.Flags().GetString("output-dir")
			keepFiles, _ := cmd.Flags().GetBool("keep-files")
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")
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

			ios.WriteOutLine("链式下载 %d 个 URL...", len(urls))

			opts := []client.ChainOption{
				client.WithChainPollInterval(pollInterval),
				client.WithChainTimeout(timeout),
			}
			if keepFiles {
				opts = append(opts, client.WithChainKeepFiles())
			}

			result, err := svc.CloudDownloadChain(cmd.Context(), urls, archiveName, outputDir, opts...)
			if err != nil {
				return fmt.Errorf("链式下载失败: %w", err)
			}

			cdc, ok := result.Raw.(*client.CloudDownloadChain)
			if ok {
				ios.WriteOutLine("链式下载完成!")
				ios.WriteOutLine("  本地路径: %s", cdc.LocalPath)
				if !cdc.KeepFiles {
					ios.WriteOutLine("  远端文件: 已清理")
				}
			} else {
				ios.WriteOutLine("链式下载完成: %s", result.ChainID)
			}
			return nil
		},
	}

	// 注册 flags
	cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
	cmd.Flags().String("output-dir", ".", "本地输出目录")
	cmd.Flags().Bool("keep-files", false, "下载到本地后不删除云端副本")
	cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "链式操作超时时间")
	cmd.Flags().Bool("no-cache", false, "不使用缓存（重新下载）")
	cmd.Flags().String("batch", "", "从文件读取 URL 列表（每行一个 URL，忽略空行和 # 注释行）")

	// 注册子命令
	cmd.AddCommand(NewCmdCloudSubmit(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudWait(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudArchive(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudFetch(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudResume(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudList(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudCancel(factory, ios, cfgSvc))

	return cmd
}

// NewCmdCloudSubmit 创建 submit 子命令，仅提交云端下载任务。
func NewCmdCloudSubmit(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "submit <url> [url...]",
		Short: "提交云端下载任务（不等待完成）",
		Long: `提交 URL 到服务端进行云端下载，返回任务 ID 和状态。
不等待任务完成，使用 wait 子命令轮询。
支持多个 URL 参数或通过 --batch 从文件读取 URL 列表。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			batchFile, _ := cmd.Flags().GetString("batch")

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

			ios.WriteOutLine("创建云端下载任务...")
			tasks, err := svc.CloudDownloadBatch(cmd.Context(), urls)
			if err != nil {
				return fmt.Errorf("创建云端下载任务失败: %w", err)
			}

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
			return nil
		},
	}

	cmd.Flags().String("batch", "", "从文件读取 URL 列表（每行一个 URL，忽略空行和 # 注释行）")
	return cmd
}

// NewCmdCloudWait 创建 wait 子命令，等待云端下载任务完成。
func NewCmdCloudWait(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait <task-id> [task-id...]",
		Short: "等待云端下载任务完成",
		Long:  `轮询等待指定云端下载任务完成，显示下载进度。`,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			// 从 wait 子命令自身的 flags 读取
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")

			// 获取初始任务状态
			tasks := make([]client.CloudTask, len(args))
			for i, id := range args {
				task, getErr := svc.GetCloudTask(cmd.Context(), id)
				if getErr != nil {
					ios.WriteErrLine("获取任务 %s 信息失败: %v", id, getErr)
					tasks[i] = client.CloudTask{ID: id, Status: "failed"}
					continue
				}
				tasks[i] = *task
			}

			// 收集待轮询任务
			pending := make(map[string]client.CloudTask)
			for _, t := range tasks {
				if t.Status == "pending" || t.Status == "downloading" {
					pending[t.ID] = t
				}
			}

			if len(pending) == 0 {
				for _, t := range tasks {
					ios.WriteOutLine("  %s: %s", t.ID, t.Status)
				}
				return nil
			}

			ios.WriteOutLine("等待 %d 个任务完成...", len(pending))

			pollCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()

			results := make(map[string]client.CloudTask)
			for _, t := range tasks {
				results[t.ID] = t
			}

			for len(pending) > 0 {
				select {
				case <-pollCtx.Done():
					if len(pending) > 0 {
						return pollCtx.Err()
					}
					return nil
				case <-ticker.C:
					for id := range pending {
						task, pollErr := svc.GetCloudTask(pollCtx, id)
						if pollErr != nil {
							ios.WriteOutLine("  轮询任务 %s 失败: %v", id, pollErr)
							delete(pending, id)
							continue
						}
						results[id] = *task
						switch task.Status {
						case "completed":
							delete(pending, id)
							ios.WriteOutLine("  ✓ %s: 完成 (%s, %d bytes)", id, task.Filename, task.TotalSize)
						case "failed":
							delete(pending, id)
							ios.WriteOutLine("  ✗ %s: 失败 - %s", id, task.Error)
						case "cancelled":
							delete(pending, id)
							ios.WriteOutLine("  ✗ %s: 已取消", id)
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
			return nil
		},
	}

	cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "等待超时时间")
	return cmd
}

// NewCmdCloudFetch 创建 fetch 子命令，执行完整链式下载。
func NewCmdCloudFetch(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fetch <url> [url...]",
		Short: "完整链式下载（提交→等待→打包→下载→清理）",
		Long: `完整链式操作：提交云端下载任务，等待完成，打包归档，下载到本地，清理远端文件。
与 cloud-download 主命令行为相同，作为子命令提供以方便在脚本中使用。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			archiveName, _ := cmd.Flags().GetString("archive-name")
			outputDir, _ := cmd.Flags().GetString("output-dir")
			keepFiles, _ := cmd.Flags().GetBool("keep-files")
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			batchFile, _ := cmd.Flags().GetString("batch")

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

			ios.WriteOutLine("链式下载 %d 个 URL...", len(urls))

			opts := []client.ChainOption{
				client.WithChainPollInterval(pollInterval),
				client.WithChainTimeout(timeout),
			}
			if keepFiles {
				opts = append(opts, client.WithChainKeepFiles())
			}

			result, err := svc.CloudDownloadChain(cmd.Context(), urls, archiveName, outputDir, opts...)
			if err != nil {
				return fmt.Errorf("链式下载失败: %w", err)
			}

			cdc, ok := result.Raw.(*client.CloudDownloadChain)
			if ok {
				ios.WriteOutLine("链式下载完成!")
				ios.WriteOutLine("  本地路径: %s", cdc.LocalPath)
				if !cdc.KeepFiles {
					ios.WriteOutLine("  远端文件: 已清理")
				}
			} else {
				ios.WriteOutLine("链式下载完成: %s", result.ChainID)
			}
			return nil
		},
	}

	cmd.Flags().String("archive-name", "", "归档文件名（默认自动生成）")
	cmd.Flags().String("output-dir", ".", "本地输出目录")
	cmd.Flags().Bool("keep-files", false, "下载到本地后不删除云端副本")
	cmd.Flags().Duration("poll-interval", 3*time.Second, "轮询间隔")
	cmd.Flags().Duration("timeout", 30*time.Minute, "链式操作超时时间")
	cmd.Flags().Bool("no-cache", false, "不使用缓存（重新下载）")
	cmd.Flags().String("batch", "", "从文件读取 URL 列表（每行一个 URL，忽略空行和 # 注释行）")

	return cmd
}

// NewCmdCloudResume 创建 resume 子命令，恢复中断的链式操作。
func NewCmdCloudResume(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <chain-id>",
		Short: "恢复中断的链式操作",
		Long: `从缓存恢复并继续执行中断的链式操作。
需要客户端配置了 cache_dir 以启用链式操作持久化。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			chainID := args[0]
			ios.WriteOutLine("恢复链式操作: %s", chainID)

			result, err := svc.ResumeChain(cmd.Context(), chainID)
			if err != nil {
				return fmt.Errorf("恢复链式操作失败: %w", err)
			}

			cdc, ok := result.Raw.(*client.CloudDownloadChain)
			if ok {
				ios.WriteOutLine("链式操作完成!")
				ios.WriteOutLine("  本地路径: %s", cdc.LocalPath)
				if !cdc.KeepFiles {
					ios.WriteOutLine("  远端文件: 已清理")
				}
			} else {
				ios.WriteOutLine("链式操作完成: %s", result.ChainID)
			}
			return nil
		},
	}

	return cmd
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
