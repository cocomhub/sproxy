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

	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var tunnelCmd = &cobra.Command{
	Use:   "tunnel [flags] <url>",
	Short: "通过加密隧道转发请求",
	Long: `通过加密隧道发送 HTTP 请求。
需要配置 tunnel_key 才能使用。

示例:
  sclient tunnel https://api.example.com/data
  sclient tunnel -X POST -H "Content-Type: application/json" -d '{"key":"val"}' https://api.example.com/echo`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// tunnel 命令可能没有通过 PersistentPreRunE，所以 fallback 初始化 cfgProvider
		if cfgProvider == nil {
			cfgProvider = sclientcfg.New(cfgFile)
			cfgProvider.BindPFlag("server_url", cmd.Flags().Lookup("server"))
		}

		cfg, err := client.LoadFromProvider(cfgProvider)
		if err != nil {
			return fmt.Errorf("加载配置失败: %w", err)
		}

		if cfg.TunnelKey == "" {
			return fmt.Errorf("请先配置 tunnel_key: sclient config set tunnel_key <64位hex密钥>")
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

		return tunnelRequest(tunnelReqOpts{
		cfg:        cfg,
		method:     method,
		targetURL:  targetURL,
		headers:    headers,
		body:       body,
		outputFile: outputPath,
		verbose:    verbose,
		include:    include,
	})
	},
}

func init() {
	tunnelCmd.Flags().StringP("method", "X", "GET", "请求方法")
	tunnelCmd.Flags().StringArrayP("header", "H", nil, "自定义请求头 (可重复)")
	tunnelCmd.Flags().StringP("data", "d", "", "请求体 (@file 从文件读取)")
	tunnelCmd.Flags().BoolP("include", "i", false, "显示响应头")
}

// tunnelReqOpts 是 tunnelRequest 的参数集合，减少函数参数数量。
type tunnelReqOpts struct {
	cfg        *client.Config
	method     string
	targetURL  string
	headers    []string
	body       string
	outputFile string
	verbose    bool
	include    bool
}

// tunnelRequest 是 CLI 专用的隧道请求函数，包含 curl 风格的进度条输出。
func tunnelRequest(opts tunnelReqOpts) error {
	// 创建带隧道配置的新客户端
	tunnelOpt := client.WithTunnel(opts.cfg.TunnelKey)
	tunnelCli := client.NewFileClient(opts.cfg.ServerURL, tunnelOpt)

	var bodyReader io.Reader
	if opts.body != "" {
		bodyReader = strings.NewReader(opts.body)
	}

	req, err := http.NewRequest(opts.method, opts.targetURL, bodyReader)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	for _, h := range opts.headers {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}

	finalOutputFile, err := resolveOutputPath(opts.targetURL, opts.outputFile)
	if err != nil {
		return err
	}

	f, err := os.Create(finalOutputFile)
	if err != nil {
		return fmt.Errorf("创建结果文件失败: %w", err)
	}
	defer f.Close()

	if opts.verbose {
		fmt.Fprintf(os.Stderr, "--%s-- #Tunnel %s/tunnel\n", time.Now().Format("2006-01-02 15:04:05"), opts.cfg.ServerURL)
		fmt.Fprintf(os.Stderr, "[请求] %s %s\n", opts.method, opts.targetURL)
		for k := range req.Header {
			fmt.Fprintf(os.Stderr, "%s: %s\n", k, req.Header.Get(k))
		}
		fmt.Fprintln(os.Stderr)
	}

	resp, err := tunnelCli.TunnelDo(req)
	if err != nil {
		return fmt.Errorf("tunnel 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if opts.include || opts.verbose {
		fmt.Fprintf(os.Stderr, "[响应状态] %s\n", resp.Status)
		for k := range resp.Header {
			fmt.Fprintf(os.Stderr, "%s: %s\n", k, resp.Header.Get(k))
		}
		fmt.Fprintln(os.Stderr)
	}

	contentLength := resp.ContentLength
	if contentLength > 0 {
		fmt.Fprintf(os.Stderr, "长度：%d (%s) [%s]\n", contentLength, client.FormatByte(float64(contentLength)), resp.Header.Get("Content-Type"))
		fmt.Fprintf(os.Stderr, "正在保存至: '%s'\n\n", finalOutputFile)
	}

	totalRead, err := writeWithProgress(resp.Body, f, contentLength)
	if err != nil {
		return err
	}

	if contentLength > 0 {
		fmt.Fprintf(os.Stderr, "\n'%s' saved [%d/%d]\n", finalOutputFile, totalRead, contentLength)
	}

	modTimeStr := resp.Header.Get("Last-Modified")
	if modTimeStr != "" {
		modTime, err := time.Parse(time.RFC1123, modTimeStr)
		if err == nil {
			_ = os.Chtimes(finalOutputFile, modTime, modTime)
		}
	}
	return nil
}

// resolveOutputPath 计算输出文件路径。若已指定 outputFile 则直接返回；
// 否则从 URL 路径提取 basename，处理同名冲突后返回。
func resolveOutputPath(targetURL, outputFile string) (string, error) {
	if outputFile != "" {
		return outputFile, nil
	}
	baseDir := currentDir
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

			if contentLength > 0 && time.Since(lastPrintAt) > time.Second {
				percent := float64(totalRead) / float64(contentLength) * 100
				filled := int(percent / 100 * float64(barWidth))
				bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
				fmt.Fprintf(os.Stderr, "\r%6.2f%% [%s] %s      ",
					percent, bar, client.FormatByte(float64(totalRead)))
				lastPrintAt = time.Now()
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return totalRead, fmt.Errorf("读取响应体失败: %w", err)
		}
	}

	if contentLength > 0 {
		endAt := time.Now()
		percent := float64(totalRead) / float64(contentLength) * 100
		filled := int(percent / 100 * float64(barWidth))
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
		fmt.Fprintf(os.Stderr, "\r%6.2f%% [%s] %s   in %s    \n",
			percent, bar, client.FormatByte(float64(totalRead)), endAt.Sub(startAt))
	}

	return totalRead, nil
}
