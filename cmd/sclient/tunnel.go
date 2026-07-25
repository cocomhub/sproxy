// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// tunnelReqOpts 是 buildTunnelRequest 的参数集合。
type tunnelReqOpts struct {
	method    string
	targetURL string
	headers   []string
	body      string
}

// buildTunnelRequest 构建 HTTP 请求并设置自定义头。
func buildTunnelRequest(opts tunnelReqOpts) (*http.Request, error) {
	var bodyReader io.Reader
	if opts.body != "" {
		bodyReader = strings.NewReader(opts.body)
	}

	req, err := http.NewRequest(opts.method, opts.targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	for _, h := range opts.headers {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}
	return req, nil
}

// resolveOutputPath 计算输出文件路径。若已指定 outputFile 则直接返回；
// 否则从 URL 路径提取 basename，处理同名冲突后返回。
func resolveOutputPath(targetURL, outputFile, baseDir string) (string, error) {
	if outputFile != "" {
		return outputFile, nil
	}
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return "", fmt.Errorf("解析 URL 失败: %w", err)
	}
	baseOutputFile := path.Base(u.Path)
	if baseOutputFile == "." || baseOutputFile == "" || baseOutputFile == "/" {
		baseOutputFile = "index.html"
	}
	finalOutputFile := filepath.Join(baseDir, baseOutputFile)
	no := 1
	for {
		if _, err := os.Stat(finalOutputFile); errors.Is(err, os.ErrNotExist) {
			break
		}
		finalOutputFile = filepath.Join(baseDir, fmt.Sprintf("%s.%d", baseOutputFile, no))
		no++
	}
	return finalOutputFile, nil
}

// NewCmdTunnel 创建隧道命令的工厂函数，使用 client.Service 接口。
func NewCmdTunnel(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel [flags] <url>",
		Short: "通过加密隧道转发请求",
		Long: `通过加密隧道发送 HTTP 请求。
	需要配置 tunnel_key 才能使用。

	示例:
	  sclient tunnel https://api.example.com/data
	  sclient tunnel -X POST -H "Content-Type: application/json" -d '{"key":"val"}' https://api.example.com/echo`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}

			method, _ := cmd.Flags().GetString("method")
			headers, _ := cmd.Flags().GetStringArray("header")
			body, _ := cmd.Flags().GetString("data")
			include, _ := cmd.Flags().GetBool("include")
			outputPath, _ := cmd.Flags().GetString("output")
			verbose, _ := cmd.Flags().GetBool("verbose")

			// 处理 @file 格式的 body
			if strings.HasPrefix(body, "@") {
				data, err := os.ReadFile(body[1:])
				if err != nil {
					return fmt.Errorf("读取文件失败: %w", err)
				}
				body = string(data)
			}

			targetURL := args[0]

			// 构造请求（复用 buildTunnelRequest）
			opts := tunnelReqOpts{
				method:    method,
				targetURL: targetURL,
				headers:   headers,
				body:      body,
			}
			req, err := buildTunnelRequest(opts)
			if err != nil {
				return err
			}

			finalOutputFile, err := resolveOutputPath(targetURL, outputPath, "")
			if err != nil {
				return err
			}

			if verbose {
				fmt.Fprintf(ios.ErrOut, "[请求] %s %s\n", method, targetURL)
				for k := range req.Header {
					fmt.Fprintf(ios.ErrOut, "%s: %s\n", k, req.Header.Get(k))
				}
				fmt.Fprintln(ios.ErrOut)
			}

			resp, err := svc.TunnelDo(req)
			if err != nil {
				return fmt.Errorf("tunnel 请求失败: %w", err)
			}
			defer resp.Body.Close()

			if include || verbose {
				fmt.Fprintf(ios.ErrOut, "[响应状态] %s\n", resp.Status)
				for k := range resp.Header {
					fmt.Fprintf(ios.ErrOut, "%s: %s\n", k, resp.Header.Get(k))
				}
				fmt.Fprintln(ios.ErrOut)
			}

			f, err := os.Create(finalOutputFile)
			if err != nil {
				return fmt.Errorf("创建结果文件失败: %w", err)
			}
			defer f.Close()

			contentLength := resp.ContentLength
			if contentLength > 0 {
				fmt.Fprintf(ios.ErrOut, "长度：%d (%s) [%s]\n",
					contentLength, client.FormatByte(float64(contentLength)), resp.Header.Get("Content-Type"))
				fmt.Fprintf(ios.ErrOut, "正在保存至: '%s'\n\n", finalOutputFile)
			}

			totalRead, err := writeWithProgress(resp.Body, f, contentLength)
			if err != nil {
				return err
			}

			if contentLength > 0 {
				fmt.Fprintf(ios.ErrOut, "\n'%s' saved [%d/%d]\n", finalOutputFile, totalRead, contentLength)
			}

			modTimeStr := resp.Header.Get("Last-Modified")
			if modTimeStr != "" {
				modTime, err := time.Parse(time.RFC1123, modTimeStr)
				if err == nil {
					_ = os.Chtimes(finalOutputFile, modTime, modTime)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringP("method", "X", "GET", "请求方法")
	cmd.Flags().StringArrayP("header", "H", nil, "自定义请求头 (可重复)")
	cmd.Flags().StringP("data", "d", "", "请求体 (@file 从文件读取)")
	cmd.Flags().BoolP("include", "i", false, "显示响应头")
	return cmd
}

// writeWithProgress 从 r 读取数据写入 w，同时以进度条形式显示进度。
// contentLength 为 -1 时不显示进度条。
func writeWithProgress(r io.Reader, w io.Writer, contentLength int64) (int64, error) {
	barWidth := 50
	var totalRead int64
	startAt := time.Now()
	lastPrintAt := time.Now()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return totalRead, fmt.Errorf("写入文件失败: %w", writeErr)
			}
			totalRead += int64(written)

			maybePrintProgress(contentLength, totalRead, barWidth, &lastPrintAt)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return totalRead, fmt.Errorf("读取响应体失败: %w", err)
		}
	}

	if contentLength > 0 {
		printProgressBarDone(os.Stderr, totalRead, contentLength, barWidth, startAt)
	}

	return totalRead, nil
}

// maybePrintProgress 如果满足条件（有总长度且距上次打印超过 1 秒）则打印进度条。
func maybePrintProgress(contentLength, totalRead int64, barWidth int, lastPrintAt *time.Time) {
	if contentLength > 0 && time.Since(*lastPrintAt) > time.Second {
		printProgressBar(os.Stderr, totalRead, contentLength, barWidth)
		*lastPrintAt = time.Now()
	}
}

// printProgressBar 在 stderr 上打印单行进度条。
func printProgressBar(w io.Writer, current, total int64, barWidth int) {
	percent := float64(current) / float64(total) * 100
	filled := int(percent / 100 * float64(barWidth))
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
	fmt.Fprintf(w, "\r%6.2f%% [%s] %s      ",
		percent, bar, client.FormatByte(float64(current)))
}

// printProgressBarDone 打印进度条终态（包含总耗时）。
func printProgressBarDone(w io.Writer, current, total int64, barWidth int, startAt time.Time) {
	endAt := time.Now()
	percent := float64(current) / float64(total) * 100
	filled := int(percent / 100 * float64(barWidth))
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
	fmt.Fprintf(w, "\r%6.2f%% [%s] %s   in %s    \n",
		percent, bar, client.FormatByte(float64(current)), endAt.Sub(startAt))
}
