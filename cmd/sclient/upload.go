// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/e2ee"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// uploadOptions 汇总 upload 命令的 flag 选项。
type uploadOptions struct {
	chunked     bool
	concurrency int
	chunkSize   int64
	resume      bool
	encrypt     bool
	e2eeKeyHex  string
}

// NewCmdUpload 创建独立的 upload 命令工厂函数，使用 clientfactory.Factory 替代 buildFileClient。
func NewCmdUpload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload <file1> [file2...]",
		Short: "上传一个或多个文件",
		Long: `上传一个或多个文件到 sproxy 服务端。
		文件路径中的目录结构会被保留。
		如：sclient upload dir/file.txt 会将文件保存到服务端的 storage_root/dir/file.txt

		受当前目录 (cd) 影响：相对路径会拼接当前目录前缀。
		使用 / 开头的绝对路径可以绕过当前目录。`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpload(cmd, args, factory, ios, st)
		},
	}

	cmd.Flags().Bool("chunked", false, "启用分块上传模式")
	cmd.Flags().Bool("encrypt", false, "客户端 E2EE：上传前 AES-256-GCM 加密（零知识）")
	cmd.Flags().String("e2ee-key", "", "客户端 E2EE 密钥（64 hex 字符 = 32B）")
	cmd.Flags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	cmd.Flags().Int("concurrency", 0, "上传并发数 (默认 4)")
	cmd.Flags().Bool("resume", true, "续传模式")

	return cmd
}

// runUpload 执行 upload 命令主体。
func runUpload(cmd *cobra.Command, args []string, factory clientfactory.Factory, ios cli.IOStreams, st *state.State) error {
	svc, err := factory.NewClient(cmd)
	if err != nil {
		ios.WriteErrLine(errFmtInitClientPrint, err)
		return fmt.Errorf(errFmtInitClient, err)
	}

	chunkedFlag, _ := cmd.Flags().GetBool("chunked")
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	chunkSize, _ := cmd.Flags().GetInt64("chunk-size")
	resume, _ := cmd.Flags().GetBool("resume")
	encrypt, _ := cmd.Flags().GetBool("encrypt")
	e2eeKeyHex, _ := cmd.Flags().GetString("e2ee-key")
	opts := uploadOptions{chunked: chunkedFlag, concurrency: concurrency, chunkSize: chunkSize, resume: resume, encrypt: encrypt, e2eeKeyHex: e2eeKeyHex}

	ctx := cmd.Context()
	// 传输统计收集（--json 输出 stats 字段；表格追加统计行）。
	stats := NewTransferStats()
	for _, filePath := range args {
		fmt.Fprintf(ios.Out, "上传: %s\n", filePath)
		if err := uploadOneFile(ctx, svc, filePath, ios, st, opts, stats); err != nil {
			return err
		}
	}
	stats.Finalize()
	// 统计行走 formatter：表格输出 FormatLine 文本；--json 输出 stats 对象。
	buildFormatterWithWriter(ios.Out, cmd).PrintTransferStats(stats)
	return nil
}

// uploadOneFile 计算远端路径并分发到分块/加密分支上传。
func uploadOneFile(ctx context.Context, svc *client.FileClient, filePath string, ios cli.IOStreams, st *state.State, opts uploadOptions, stats *TransferStats) error {
	useChunked := opts.chunked
	if !useChunked {
		if stat, err := os.Stat(filePath); err == nil {
			useChunked = client.ShouldAutoChunk(stat.Size())
		}
	}

	remotePath, err := st.ResolveRemotePathOrErr(filepath.ToSlash(filepath.Clean(filePath)))
	if err != nil {
		return err
	}
	fmt.Fprintf(ios.Out, "远端路径: %s\n", remotePath)

	if useChunked {
		return uploadChunked(ctx, svc, filePath, remotePath, ios, opts, stats)
	}
	return uploadPlain(ctx, svc, filePath, remotePath, ios, opts, stats)
}

// uploadChunked 以分块模式上传单个文件并把结果写入传输统计。
func uploadChunked(ctx context.Context, svc *client.FileClient, filePath, remotePath string, ios cli.IOStreams, opts uploadOptions, stats *TransferStats) error {
	chunkOpts := []client.ChunkedOption{
		client.WithChunkedResume(opts.resume),
	}
	if opts.chunkSize > 0 {
		chunkOpts = append(chunkOpts, client.WithChunkedChunkSize(opts.chunkSize))
	}
	if opts.concurrency > 0 {
		chunkOpts = append(chunkOpts, client.WithChunkedConcurrency(opts.concurrency))
	}
	fileStart := time.Now()
	result, err := svc.ChunkedUpload(ctx, filePath, remotePath, chunkOpts...)
	if err != nil {
		fmt.Fprintf(ios.ErrOut, "分块上传失败: %s %v\n", filePath, err)
		return fmt.Errorf("分块上传失败 %s: %w", filePath, err)
	}
	// 分块成功率：totalChunks 已知 + 最终 mismatch 为空 = 全成功；
	// 失败路径已 return，此处成功分支 mismatch 恒为空（成功率 100%）。
	if result.TotalChunks > 0 {
		stats.SetChunkSuccessRate(result.TotalChunks, len(result.MismatchChunks))
	}
	stats.AddFile(remotePath, fileSizeOr(filePath), time.Since(fileStart))
	fmt.Fprintf(ios.Out, "成功: %v, 消息: %s\n", result.Success, result.Message)
	if result.FileChecksum != "" {
		fmt.Fprintf(ios.Out, "文件 SHA-256: %s\n", result.FileChecksum)
	}
	return nil
}

// uploadPlain 以普通模式上传单个文件（可选客户端 E2EE 加密）。
func uploadPlain(ctx context.Context, svc *client.FileClient, filePath, remotePath string, ios cli.IOStreams, opts uploadOptions, stats *TransferStats) error {
	fileStart := time.Now()
	upPath := filePath
	if opts.encrypt {
		key, kerr := e2eeKeyFromHex(opts.e2eeKeyHex)
		if kerr != nil {
			return kerr
		}
		upPath, kerr = encryptFileToTemp(filePath, key)
		if kerr != nil {
			return kerr
		}
		defer os.Remove(upPath)
	}
	result, err := svc.Upload(ctx, upPath, remotePath)
	if err != nil {
		fmt.Fprintf(ios.ErrOut, "上传失败: %s %v\n", filePath, err)
		if result != nil {
			fmt.Fprintf(ios.ErrOut, "服务端消息: %s\n", result.Message)
		}
		return fmt.Errorf("上传失败 %s: %w", filePath, err)
	}
	stats.AddFile(remotePath, fileSizeOr(filePath), time.Since(fileStart))
	fmt.Fprintf(ios.Out, "成功: %v, 消息: %s\n", result.Success, result.Message)
	if result.Checksum != "" {
		fmt.Fprintf(ios.Out, "文件 SHA-256: %s\n", result.Checksum)
	}
	return nil
}

// fileSizeOr 返回本地文件的字节大小；stat 失败返回 0（传输统计尽力而为，不阻塞上传）。
func fileSizeOr(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// e2eeKeyFromHex 解析 64 hex 字符密钥（32B）。
func e2eeKeyFromHex(hexStr string) ([]byte, error) {
	if len(hexStr) != 64 {
		return nil, fmt.Errorf("e2ee: 密钥必须 64 hex 字符（32B）")
	}
	key, err := hex.DecodeString(hexStr)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("e2ee: 密钥必须 64 hex 字符（32B）")
	}
	return key, nil
}

// encryptFileToTemp 把文件加密写入临时文件（上传 E2EE）。
func encryptFileToTemp(src string, key []byte) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("e2ee: 打开源文件: %w", err)
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "sproxy-e2ee-*.enc")
	if err != nil {
		return "", fmt.Errorf("e2ee: 创建临时文件: %w", err)
	}
	ew, err := e2ee.NewEncryptWriter(key, tmp)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if _, err := io.Copy(ew, f); err != nil {
		ew.Close()
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("e2ee: 加密失败: %w", err)
	}
	if err := ew.Close(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
