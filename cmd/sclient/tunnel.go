// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
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
		v := viper.New()
		v.SetConfigFile(cfgFile)
		v.SetConfigType("yaml")
		v.SetEnvPrefix("SCLIENT")
		v.AutomaticEnv()
		if err := v.ReadInConfig(); err != nil {
			var re viper.ConfigFileNotFoundError
			if !errors.As(err, &re) {
				return fmt.Errorf("读取配置文件失败: %w", err)
			}
		}
		_ = v.BindPFlag("server_url", cmd.Flags().Lookup("server"))

		cfg, err := client.LoadFromViper(v)
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

		return tunnelRequest(cfg, method, targetURL, headers, body, outputPath, verbose, include)
	},
}

func init() {
	tunnelCmd.Flags().StringP("method", "X", "GET", "请求方法")
	tunnelCmd.Flags().StringArrayP("header", "H", nil, "自定义请求头 (可重复)")
	tunnelCmd.Flags().StringP("data", "d", "", "请求体 (@file 从文件读取)")
	tunnelCmd.Flags().BoolP("include", "i", false, "显示响应头")
}

// tunnelRequest 是 CLI 专用的隧道请求函数，包含 curl 风格的进度条输出。
func tunnelRequest(cfg *client.Config, method, targetURL string, headers []string, body, outputFile string, verbose, include bool) error {
	// 创建带隧道配置的新客户端
	tunnelOpt := client.WithTunnel(cfg.TunnelKey)
	tunnelCli := client.NewFileClient(cfg.ServerURL, tunnelOpt)

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, targetURL, bodyReader)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	for _, h := range headers {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}

	finalOutputFile := outputFile
	if finalOutputFile == "" {
		baseOutputFile := path.Base(req.URL.Path)
		if baseOutputFile == "." || baseOutputFile == "" || baseOutputFile == "/" {
			baseOutputFile = "index.html"
		}
		finalOutputFile = baseOutputFile
		no := 1
		for {
			if _, err := os.Stat(finalOutputFile); errors.Is(err, os.ErrNotExist) {
				break
			}
			finalOutputFile = fmt.Sprintf("%s.%d", baseOutputFile, no)
			no++
		}
	}

	f, err := os.Create(finalOutputFile)
	if err != nil {
		return fmt.Errorf("创建结果文件失败: %w", err)
	}
	defer f.Close()

	if verbose {
		fmt.Fprintf(os.Stderr, "--%s-- #Tunnel %s/tunnel\n", time.Now().Format("2006-01-02 15:04:05"), cfg.ServerURL)
		fmt.Fprintf(os.Stderr, "[请求] %s %s\n", method, targetURL)
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

	if include || verbose {
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

	barWidth := 50
	var totalRead int64
	startAt := time.Now()
	lastPrintAt := time.Now()
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			written, writeErr := f.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("写入文件失败: %w", writeErr)
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
			return fmt.Errorf("读取响应体失败: %w", err)
		}
	}

	if contentLength > 0 {
		endAt := time.Now()
		percent := float64(totalRead) / float64(contentLength) * 100
		filled := int(percent / 100 * float64(barWidth))
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
		fmt.Fprintf(os.Stderr, "\r%6.2f%% [%s] %s   in %s    \n",
			percent, bar, client.FormatByte(float64(totalRead)), endAt.Sub(startAt))
		fmt.Fprintf(os.Stderr, "\n'%s' saved [%d/%d]\n", finalOutputFile, totalRead, contentLength)
	}

	modTimeStr := resp.Header.Get("Last-Modified")
	if modTimeStr != "" {
		modTime, err := time.Parse(time.RFC1123, modTimeStr)
		if err == nil {
			os.Chtimes(finalOutputFile, modTime, modTime)
		}
	}
	return nil
}
