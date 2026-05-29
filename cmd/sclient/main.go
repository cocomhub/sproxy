// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

var cfgPath string

func init() {
	configPath, err := configFilePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取配置文件路径失败: %v\n", err)
		os.Exit(1)
	}
	flag.StringVar(&cfgPath, "config", configPath, "配置文件路径")
}

func configFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败: %w", err)
	}
	return filepath.Join(home, ".sclient.yaml"), nil
}

func main() {
	flag.Parse()
	args := flag.Args()

	if len(args) == 0 {
		printHelp()
		os.Exit(0)
	}

	cmd, cmdArgs := parseCommand(args)

	var serverOverride string
	var noChecksum bool
	var outputPath string
	var verbose bool
	var chunkedMode bool
	var chunkSize int64
	var concurrency int
	var resume bool

	remaining := parseGlobalOptions(cmdArgs, &serverOverride, &noChecksum, &outputPath, &verbose, &chunkedMode, &chunkSize, &concurrency, &resume)

	cfg, err := client.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	serverURL := cfg.ServerURL
	if serverOverride != "" {
		serverURL = serverOverride
	}

	// 构造 FileClient
	logger := initLogger(verbose)
	opts := []client.Option{
		client.WithLogger(logger),
		client.WithChecksum(!noChecksum),
		client.WithProgress(func(label string, read, total int64) {
			if total > 0 {
				percent := float64(read) / float64(total) * 100
				fmt.Fprintf(os.Stderr, "\r%s: %.1f%% (%s/%s)  ", label, percent, client.FormatByte(float64(read)), client.FormatByte(float64(total)))
			} else {
				fmt.Fprintf(os.Stderr, "\r%s: %s  ", label, client.FormatByte(float64(read)))
			}
			if read == total {
				fmt.Fprintf(os.Stderr, "\n")
			}
		}),
	}
	if cfg.TunnelKey != "" {
		opts = append(opts, client.WithTunnel(cfg.TunnelKey))
	}
	if cfg.ChunkSize > 0 {
		opts = append(opts, func(c *client.FileClient) {
			c.ChunkSize = cfg.ChunkSize
		})
	}
	cli := client.NewFileClient(serverURL, opts...)
	ctx := context.Background()

	switch cmd {
	case "upload":
		if len(remaining) == 0 {
			fmt.Fprintln(os.Stderr, "请指定要上传的文件")
			os.Exit(1)
		}
		for _, filePath := range remaining {
			fmt.Printf("上传: %s\n", filePath)
			// 判断是否使用分块上传
			useChunked := chunkedMode
			if !useChunked {
				if stat, err := os.Stat(filePath); err == nil {
					useChunked = client.ShouldAutoChunk(stat.Size())
				}
			}
			if useChunked {
				chunkOpts := []client.ChunkedOption{
					client.WithChunkedResume(resume),
				}
				if chunkSize > 0 {
					chunkOpts = append(chunkOpts, client.WithChunkedChunkSize(chunkSize))
				}
				if concurrency > 0 {
					chunkOpts = append(chunkOpts, client.WithChunkedConcurrency(concurrency))
				}
				result, err := cli.ChunkedUpload(ctx, filePath, chunkOpts...)
				if err != nil {
					fmt.Fprintf(os.Stderr, "分块上传失败: %s %v\n", filePath, err)
					os.Exit(1)
				}
				fmt.Printf("成功: %v, 消息: %s\n", result.Success, result.Message)
				if result.FileChecksum != "" {
					fmt.Printf("文件 SHA-256: %s\n", result.FileChecksum)
				}
			} else {
				result, err := cli.Upload(ctx, filePath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "上传失败: %s %v\n", filePath, err)
					if result != nil {
						fmt.Fprintf(os.Stderr, "服务端消息: %s\n", result.Message)
					}
					os.Exit(1)
				}
				fmt.Printf("成功: %v, 消息: %s\n", result.Success, result.Message)
				if result.Checksum != "" {
					fmt.Printf("文件 SHA-256: %s\n", result.Checksum)
				}
			}
		}
	case "download":
		if len(remaining) == 0 {
			fmt.Fprintln(os.Stderr, "请指定要下载的文件名")
			os.Exit(1)
		}
		filename := remaining[0]
		if outputPath == "" && len(remaining) > 1 {
			outputPath = remaining[1]
		}

		if chunkedMode {
			chunkOpts := []client.ChunkedOption{
				client.WithChunkedResume(resume),
			}
			if chunkSize > 0 {
				chunkOpts = append(chunkOpts, client.WithChunkedChunkSize(chunkSize))
			}
			if concurrency > 0 {
				chunkOpts = append(chunkOpts, client.WithChunkedConcurrency(concurrency))
			}
			if err := cli.ChunkedDownload(ctx, filename, outputPath, chunkOpts...); err != nil {
				fmt.Fprintf(os.Stderr, "分块下载失败: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Printf("下载请求 GET %s?filename=%s\n", strings.TrimRight(serverURL, "/")+"/download", filename)
			if err := cli.Download(ctx, filename, outputPath); err != nil {
				fmt.Fprintf(os.Stderr, "下载失败: %v\n", err)
				os.Exit(1)
			}
		}
		fmt.Printf("文件已下载到: %s\n", outputPath)
	case "delete":
		if len(remaining) == 0 {
			fmt.Fprintln(os.Stderr, "请指定要删除的文件名")
			os.Exit(1)
		}
		filename := remaining[0]
		fmt.Printf("删除请求 POST %s?filename=%s\n", strings.TrimRight(serverURL, "/")+"/delete", filename)
		if err := cli.Delete(ctx, filename); err != nil {
			fmt.Fprintf(os.Stderr, "删除失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("文件删除成功: %s\n", filename)
	case "list":
		files, err := cli.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "列出文件失败: %v\n", err)
			os.Exit(1)
		}
		if len(files) == 0 {
			fmt.Println("no files found")
		} else {
			for _, f := range files {
				csPrefix := f.Checksum
				if len(csPrefix) > 16 {
					csPrefix = csPrefix[:16] + "…"
				}
				if csPrefix == "" {
					csPrefix = "-"
				}
				fmt.Printf("%-40s  %10s  %s\n", f.Name, client.FormatByte(float64(f.Size)), csPrefix)
			}
		}
	case "config":
		if len(remaining) == 0 {
			client.HandleConfigShow(cfg)
		} else {
			subCmd := remaining[0]
			switch subCmd {
			case "show":
				client.HandleConfigShow(cfg)
			case "set":
				if len(remaining) < 3 {
					fmt.Fprintln(os.Stderr, "用法: sclient config set <键> <值>")
					os.Exit(1)
				}
				if err := client.HandleConfigSet(cfg, cfgPath, remaining[1], remaining[2]); err != nil {
					fmt.Fprintf(os.Stderr, "设置配置失败: %v\n", err)
					os.Exit(1)
				}
			default:
				fmt.Fprintf(os.Stderr, "未知的 config 子命令: %s\n", subCmd)
				os.Exit(1)
			}
		}
	case "tunnel":
		if cfg.TunnelKey == "" {
			fmt.Fprintln(os.Stderr, "请先配置 tunnel_key: sclient config set tunnel_key <64位hex密钥>")
			os.Exit(1)
		}
		method := "GET"
		var headers map[string]string
		var body string
		tunnelVerbose := verbose

		var tunnelArgs []string
		i := 0
		for i < len(remaining) {
			arg := remaining[i]
			switch arg {
			case "-X", "--method":
				i++
				if i < len(remaining) {
					method = remaining[i]
				}
			case "-H", "--header":
				i++
				if i < len(remaining) {
					parts := strings.SplitN(remaining[i], ":", 2)
					if len(parts) == 2 {
						if headers == nil {
							headers = make(map[string]string)
						}
						headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
					}
				}
			case "-d", "--data":
				i++
				if i < len(remaining) {
					body = remaining[i]
				}
			default:
				if strings.HasPrefix(arg, "-") {
					fmt.Fprintf(os.Stderr, "未知选项: %s\n", arg)
					os.Exit(1)
				}
				tunnelArgs = append(tunnelArgs, arg)
			}
			i++
		}

		if len(tunnelArgs) == 0 {
			fmt.Fprintln(os.Stderr, "请指定目标 URL")
			os.Exit(1)
		}
		targetURL := tunnelArgs[0]

		if strings.HasPrefix(body, "@") {
			data, err := os.ReadFile(body[1:])
			if err != nil {
				fmt.Fprintf(os.Stderr, "读取文件失败: %v\n", err)
				os.Exit(1)
			}
			body = string(data)
		}

		if err := tunnelRequest(cfg, method, targetURL, headers, body, outputPath, tunnelVerbose); err != nil {
			fmt.Fprintf(os.Stderr, "tunnel 请求失败: %v\n", err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("sclient version %s (build: %s)\n", Version, BuildAt)
		fmt.Println()
		client.HandleConfigShow(cfg)
	case "genkey":
		key, err := tunnel.GenerateKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "生成密钥失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(key)
	case "help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n", cmd)
		fmt.Fprintln(os.Stderr, "使用 'sclient help' 查看可用命令")
		os.Exit(1)
	}
}

// tunnelRequest 是 CLI 专用的隧道请求函数，包含 curl 风格的进度条输出。
func tunnelRequest(cfg *client.Config, method, targetURL string, headers map[string]string, body, outputFile string, verbose bool) error {
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
	for k, v := range headers {
		req.Header.Set(k, v)
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
		tunnelURL := strings.TrimRight(cfg.ServerURL, "/") + cfg.TunnelEndpoint
		fmt.Fprintf(os.Stderr, "--%s-- #Tunnel %s\n", time.Now().Format("2006-01-02 15:04:05"), tunnelURL)
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

	if verbose {
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
	var totalRead, lastRead int64
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
				speed := (float64(totalRead - lastRead)) / time.Since(lastPrintAt).Seconds()
				eta := int64(float64(contentLength-totalRead) / speed)
				filled := int(percent / 100 * float64(barWidth))
				bar := strings.Repeat("=", max(filled-1, 0)) + ">" + strings.Repeat(" ", barWidth-filled)
				fmt.Fprintf(os.Stderr, "\r%6.2f%% [%s] %s (%s/s) ETA: %s      ",
					percent, bar, client.FormatByte(float64(totalRead)), client.FormatByte(speed), client.FormatETA(eta))
				lastRead = totalRead
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
		speed := (float64(totalRead - lastRead)) / time.Since(lastPrintAt).Seconds()
		filled := int(percent / 100 * float64(barWidth))
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
		fmt.Fprintf(os.Stderr, "\r%6.2f%% [%s] %s (%s/s)   in %s    ",
			percent, bar, client.FormatByte(float64(totalRead)), client.FormatByte(speed), endAt.Sub(startAt))
		totalSpeed := float64(totalRead) / endAt.Sub(startAt).Seconds()
		fmt.Fprintf(os.Stderr, "\n\n%s (%s/s) - '%s' saved [%d/%d]\n", endAt.Format("2006-01-02 15:04:05"),
			client.FormatByte(totalSpeed), finalOutputFile, totalRead, contentLength)
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

func parseCommand(args []string) (string, []string) {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:]
		}
	}
	return "", args
}

func parseGlobalOptions(args []string, serverOverride *string, noChecksum *bool, outputPath *string, verbose *bool, chunkedMode *bool, chunkSize *int64, concurrency *int, resume *bool) []string {
	var positional []string
	i := 0
	for i < len(args) {
		arg := args[i]
		switch arg {
		case "-s", "--server":
			i++
			if i < len(args) {
				*serverOverride = args[i]
			}
		case "--no-checksum":
			*noChecksum = true
		case "-o", "--output":
			i++
			if i < len(args) {
				*outputPath = args[i]
			}
		case "-v", "--verbose":
			*verbose = true
		case "--chunked":
			*chunkedMode = true
		case "--chunk-size":
			i++
			if i < len(args) {
				val := int64(0)
				if _, err := fmt.Sscanf(args[i], "%d", &val); err == nil {
					*chunkSize = val
				}
			}
		case "--concurrency":
			i++
			if i < len(args) {
				val := 0
				if _, err := fmt.Sscanf(args[i], "%d", &val); err == nil {
					*concurrency = val
				}
			}
		case "--resume":
			*resume = true
		case "-X", "--method":
			i++
			if i < len(args) {
				positional = append(positional, arg, args[i])
			}
		case "-H", "--header":
			i++
			if i < len(args) {
				positional = append(positional, arg, args[i])
			}
		case "-d", "--data":
			i++
			if i < len(args) {
				positional = append(positional, arg, args[i])
			}
		case "-i", "--include":
			positional = append(positional, arg)
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(os.Stderr, "未知选项: %s\n", arg)
				os.Exit(1)
			}
			positional = append(positional, arg)
		}
		i++
	}
	return positional
}

func printHelp() {
	fmt.Printf("文件上传下载客户端 v%s\n\n", Version)
	fmt.Println("用法: sclient <命令> [选项] [参数]")
	fmt.Println()
	fmt.Println("命令:")
	fmt.Println("  upload   <文件1> [文件2...]  上传一个或多个文件")
	fmt.Println("  download <文件名> [输出路径]  下载文件")
	fmt.Println("  delete   <文件名>            删除文件")
	fmt.Println("  list                         列出服务器上的文件")
	fmt.Println("  tunnel   <url>               通过加密隧道转发请求")
	fmt.Println("  genkey                       生成 tunnel_key 密钥")
	fmt.Println("  config   [show|set <键> <值>] 配置管理")
	fmt.Println("  version                      显示版本信息")
	fmt.Println("  help                         显示此帮助信息")
	fmt.Println()
	fmt.Println("选项:")
	fmt.Println("  -s, --server <URL>          服务器地址 (默认: http://localhost:18083)")
	fmt.Println("  --no-checksum              禁用 SHA-256 校验")
	fmt.Println("  -o, --output <路径>         指定下载文件的输出路径")
	fmt.Println("  -v, --verbose               显示详细输出")
	fmt.Println("  --chunked                  启用分块上传/下载模式")
	fmt.Println("  --chunk-size <bytes>        分块大小 (默认: 4MB)")
	fmt.Println("  --concurrency <n>           上传/下载并发数 (默认: 4)")
	fmt.Println("  --resume                   续传模式 (默认启用)")
	fmt.Println()
	fmt.Println("隧道选项:")
	fmt.Println("  -X, --method <METHOD>        请求方法 (默认: GET)")
	fmt.Println("  -H, --header <Header: Value> 自定义请求头 (可重复)")
	fmt.Println("  -d, --data <body|@file>      请求体 (@file 从文件读取)")
	fmt.Println("  -i, --include                显示响应头")
	fmt.Println()
	fmt.Println("示例:")
	fmt.Println("  sclient upload document.pdf")
	fmt.Println("  sclient upload image1.jpg image2.png")
	fmt.Println("  sclient download report.pdf")
	fmt.Println("  sclient download report.pdf -o /tmp/report.pdf")
	fmt.Println("  sclient upload data.txt -s http://192.168.1.100:18083")
	fmt.Println("  sclient upload --chunked largefile.iso")
	fmt.Println("  sclient upload --chunked --resume largefile.iso")
	fmt.Println("  sclient download --chunked largefile.iso")
	fmt.Println("  sclient config set server_url http://example.com:18083")
	fmt.Println("  sclient config show")
	fmt.Println("  sclient tunnel https://api.example.com/data")
	fmt.Println("  sclient tunnel -X POST -H \"Content-Type: application/json\" -d '{\"key\":\"val\"}' https://api.example.com/echo")
	fmt.Println()
	fmt.Printf("配置文件: %s\n", cfgPath)
}

// initLogger 初始化 sclient 的控制台日志。
// verbose 为 true 时输出 Debug 级别，否则 Info 级别。
// 输出到 stderr，格式为文本，不对用户可见输出造成干扰。
func initLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
