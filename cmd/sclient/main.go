// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

func main() {
	args := os.Args[1:]

	if len(args) == 0 {
		printHelp()
		os.Exit(0)
	}

	cmd, cmdArgs := parseCommand(args)

	var serverOverride string
	var noMD5 bool
	var outputPath string
	var verbose bool

	remaining := parseGlobalOptions(cmdArgs, &serverOverride, &noMD5, &outputPath, &verbose)

	configPath, err := configFilePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取配置文件路径失败: %v\n", err)
		os.Exit(1)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	if serverOverride != "" {
		cfg.ServerURL = serverOverride
	}
	if noMD5 {
		cfg.CheckMD5 = false
	}

	switch cmd {
	case "upload":
		if len(remaining) == 0 {
			fmt.Fprintln(os.Stderr, "请指定要上传的文件")
			os.Exit(1)
		}
		for _, filePath := range remaining {
			uploadURL := strings.TrimRight(cfg.ServerURL, "/") + cfg.UploadEndpoint
			if err := UploadFile(uploadURL, filePath, cfg.CheckMD5, verbose, cfg.Timeout); err != nil {
				fmt.Fprintf(os.Stderr, "上传 %s 失败: %v\n", filePath, err)
				os.Exit(1)
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
		downloadURL := strings.TrimRight(cfg.ServerURL, "/") + cfg.DownloadEndpoint
		if err := DownloadFile(downloadURL, filename, outputPath, cfg.CheckMD5, verbose, cfg.Timeout); err != nil {
			fmt.Fprintf(os.Stderr, "下载失败: %v\n", err)
			os.Exit(1)
		}
	case "delete":
		if len(remaining) == 0 {
			fmt.Fprintln(os.Stderr, "请指定要删除的文件名")
			os.Exit(1)
		}
		filename := remaining[0]
		deleteURL := strings.TrimRight(cfg.ServerURL, "/") + cfg.DeleteEndpoint
		if err := DeleteFile(deleteURL, filename, verbose, cfg.Timeout); err != nil {
			fmt.Fprintf(os.Stderr, "删除失败: %v\n", err)
			os.Exit(1)
		}
	case "list":
		listURL := strings.TrimRight(cfg.ServerURL, "/") + "/api/files"
		if err := ListFiles(listURL, cfg.Timeout); err != nil {
			fmt.Fprintf(os.Stderr, "列出文件失败: %v\n", err)
			os.Exit(1)
		}
	case "config":
		if len(remaining) == 0 {
			HandleConfigShow(cfg)
		} else {
			subCmd := remaining[0]
			switch subCmd {
			case "show":
				HandleConfigShow(cfg)
			case "set":
				if len(remaining) < 3 {
					fmt.Fprintln(os.Stderr, "用法: sclient config set <键> <值>")
					os.Exit(1)
				}
				if err := HandleConfigSet(cfg, configPath, remaining[1], remaining[2]); err != nil {
					fmt.Fprintf(os.Stderr, "设置配置失败: %v\n", err)
					os.Exit(1)
				}
			default:
				fmt.Fprintf(os.Stderr, "未知的 config 子命令: %s\n", subCmd)
				os.Exit(1)
			}
		}
	case "version":
		fmt.Printf("sclient version %s (build: %s)\n", Version, BuildAt)
		fmt.Println()
		HandleConfigShow(cfg)
	case "help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n", cmd)
		fmt.Fprintln(os.Stderr, "使用 'sclient help' 查看可用命令")
		os.Exit(1)
	}
}

func parseCommand(args []string) (string, []string) {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:]
		}
	}
	return "", args
}

func parseGlobalOptions(args []string, serverOverride *string, noMD5 *bool, outputPath *string, verbose *bool) []string {
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
		case "--no-md5":
			*noMD5 = true
		case "-o", "--output":
			i++
			if i < len(args) {
				*outputPath = args[i]
			}
		case "-v", "--verbose":
			*verbose = true
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
	fmt.Println("  config   [show|set <键> <值>] 配置管理")
	fmt.Println("  version                      显示版本信息")
	fmt.Println("  help                         显示此帮助信息")
	fmt.Println()
	fmt.Println("选项:")
	fmt.Println("  -s, --server <URL>          服务器地址 (默认: http://localhost:18080)")
	fmt.Println("  --no-md5                    禁用 MD5 校验")
	fmt.Println("  -o, --output <路径>         指定下载文件的输出路径")
	fmt.Println("  -v, --verbose               显示详细输出")
	fmt.Println()
	fmt.Println("示例:")
	fmt.Println("  sclient upload document.pdf")
	fmt.Println("  sclient upload image1.jpg image2.png")
	fmt.Println("  sclient download report.pdf")
	fmt.Println("  sclient download report.pdf -o /tmp/report.pdf")
	fmt.Println("  sclient upload data.txt -s http://192.168.1.100:18080")
	fmt.Println("  sclient config set server_url http://example.com:18080")
	fmt.Println("  sclient config show")
	fmt.Println()
	fmt.Println("配置文件: ~/.sclient.yaml")
}
