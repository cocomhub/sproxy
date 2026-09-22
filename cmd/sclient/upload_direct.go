// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/spf13/cobra"
)

// NewCmdUploadDirect 创建 `upload-direct <backend> <file> [file2...]` 命令：
// 经服务端签发 PUT 预签名 URL → 客户端直传 S3 → 服务端 complete 登记。
// 大文件绕过服务端带宽（roadmap 3.3 签名 v4 直传闭环）。
func NewCmdUploadDirect(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	var remotePath string
	cmd := &cobra.Command{
		Use:   "upload-direct <backend> <file1> [file2...]",
		Short: "直传文件到外部后端（S3 预签名，绕过服务端）",
		Long: `经服务端签发 PUT 预签名 URL，客户端直传对象存储（当前 s3），完成后服务端登记。
		<backend> 是已注册后端类型（GET /api/backends 可查，如 s3）。
		受当前目录 (cd) 影响：相对路径会拼接当前目录前缀。
		如：sclient upload-direct s3 dir/file.txt`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			backend := args[0]
			ctx := cmd.Context()
			for _, filePath := range args[1:] {
				rel := remotePath
				if rel == "" {
					rel = filepath.ToSlash(filepath.Clean(filePath))
				}
				if resolved, err := st.ResolveRemotePathOrErr(rel); err == nil {
					rel = resolved
				}
				if err := directUpload(ctx, svc, backend, filePath, rel, ios); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&remotePath, "remote-path", "", "服务端卷内目标路径（默认 = 本地相对路径）")
	return cmd
}

// directUpload 单个文件：签发 → 直传 → 登记。
// directUploadMaxRetries 是直传 PUT 失败的最大重试次数（前 N-1 次失败 + 最后一次）。
const directUploadMaxRetries = 3

// directUploadRetryDelay 是第 attempt 次重试前的退避延迟（指数：1s、2s、4s）。
// 测试注入为毫秒级（见 upload_direct_retry_test.go init）。
var directUploadRetryDelay = func(attempt int) time.Duration {
	return time.Duration(1<<attempt) * time.Second
}

// directUpload 单个文件：签发 → 直传 → 登记。PUT 5xx/网络失败自动重试
// （指数退避 + 每次重试前重新签发——presigned URL 有有效期，过期需新签名）。
func directUpload(ctx context.Context, svc *client.FileClient, backend, localPath, rel string, ios cli.IOStreams) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开文件: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat 文件: %w", err)
	}
	fileSize := st.Size()

	// 隔离 transport（不共享默认客户端——测试铁律；netutil.IsolatedTransport 无全局状态）。
	putClient := &http.Client{Transport: netutil.IsolatedTransport()}
	defer putClient.CloseIdleConnections()

	var putErr error
	for attempt := 1; attempt <= directUploadMaxRetries; attempt++ {
		putErr = nil
		// 1. 签发 PUT 预签名 URL（每次重试重新签发——防 URL 过期）。
		var presign struct {
			URL string `json:"url"`
		}
		if err := svc.DoJSON(ctx, "POST", "/api/backends/"+backend+"/presign?path="+rel+"&method=PUT", nil, &presign); err != nil {
			return fmt.Errorf("签发预签名 URL: %w", err)
		}
		if presign.URL == "" {
			return fmt.Errorf("服务端返回空预签名 URL")
		}

		// 2. 直传 S3（普通 HTTP PUT；预签名 URL 自带鉴权）。**每次重试用新文件句柄**
		// （http.Transport 会 Close *os.File 请求体——重试必须重开，不能复用已关句柄）。
		putFile, perr := os.Open(localPath)
		if perr != nil {
			return fmt.Errorf("重开文件: %w", perr)
		}
		putReq, perr := http.NewRequestWithContext(ctx, http.MethodPut, presign.URL, putFile)
		if perr != nil {
			putFile.Close()
			return fmt.Errorf("构造 PUT 请求: %w", perr)
		}
		putReq.ContentLength = fileSize
		putReq.Header.Set("Content-Type", "application/octet-stream")
		putResp, perr := putClient.Do(putReq)
		_ = putFile.Close() // 请求体已消费（成功或失败），句柄回收（幂等）
		if perr == nil {
			_, _ = io.Copy(io.Discard, putResp.Body)
			putResp.Body.Close()
			if putResp.StatusCode >= 300 {
				if putResp.StatusCode >= 500 {
					putErr = fmt.Errorf("直传失败 %d（可重试）", putResp.StatusCode)
				} else {
					b, _ := io.ReadAll(io.LimitReader(putResp.Body, 4096))
					return fmt.Errorf("直传失败 %d: %s", putResp.StatusCode, string(b))
				}
			}
		} else {
			putErr = fmt.Errorf("直传 PUT: %w", perr)
		}
		if putErr != nil && attempt < directUploadMaxRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(directUploadRetryDelay(attempt)):
			}
			continue
		}
		break
	}
	if putErr != nil {
		return putErr
	}

	// 3. complete 登记（服务端确认对象存在）。
	var completeResp map[string]string
	if err := svc.DoJSON(ctx, "POST", "/api/backends/"+backend+"/presign/complete?path="+rel, nil, &completeResp); err != nil {
		return fmt.Errorf("直传登记: %w", err)
	}
	fmt.Fprintf(ios.Out, "直传完成: %s → %s/%s\\n", localPath, backend, rel)
	return nil
}
