// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var archiveCmd = &cobra.Command{
	Use:   "archive [flags] <file...>",
	Short: "将服务端文件打包下载为 tar.gz",
	Long:  `将指定文件列表打包下载为 tar.gz 归档文件。支持目录。`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("至少需要一个文件名")
		}
		output, _ := cmd.Flags().GetString("output")
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}
		files := args
		if len(files) == 1 && strings.HasPrefix(files[0], "@") {
			return archiveFromFile(cli, files[0][1:], output)
		}
		return cli.Archive(cmd.Context(), files, output)
	},
}

var archiveDirCmd = &cobra.Command{
	Use:   "archive-dir [flags] <dirname>",
	Short: "将服务端目录打包下载为 tar.gz",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("需要指定目录名")
		}
		output, _ := cmd.Flags().GetString("output")
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}
		return cli.ArchiveDir(cmd.Context(), args[0], output)
	},
}

func init() {
	archiveCmd.Flags().StringP("output", "o", "archive.tar.gz", "输出文件路径")
	archiveDirCmd.Flags().StringP("output", "o", "archive.tar.gz", "输出文件路径")
	rootCmd.AddCommand(archiveCmd)
	rootCmd.AddCommand(archiveDirCmd)
}

// archiveFromFile 从文件中读取文件列表并打包下载。
func archiveFromFile(cli *client.FileClient, filePath, output string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开文件列表失败: %w", err)
	}
	defer f.Close()

	var files []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			files = append(files, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取文件列表失败: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("文件列表中无有效条目")
	}
	return cli.Archive(context.Background(), files, output)
}
