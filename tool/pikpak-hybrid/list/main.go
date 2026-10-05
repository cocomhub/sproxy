// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// PikPak 分享解析工具：从分享 URL 递归解析所有文件/视频的 stat 与直链。
// 用法：go run ./tool/... --url <分享URL> [--kind video|all] [--max 10]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

func main() {
	var shareURL string
	var kindFilter string
	var maxFiles int
	flag.StringVar(&shareURL, "url", "", "PikPak 分享 URL")
	flag.StringVar(&kindFilter, "kind", "video", "video=只看视频 | all=全部文件")
	flag.IntVar(&maxFiles, "max", 0, "最多显示条数（0=全部）")
	flag.Parse()
	if shareURL == "" {
		log.Fatal("-url 必填（PikPak 分享链接，如 https://mypikpak.com/s/xxxx）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	// 注：不用 defer cancel——gocritic exitAfterDefer（Fatalf 分支跳过 defer）；
	// 改为所有退出点（含正常收尾）显式 cancel。
	r := pikpak.NewShareResolver(pikpak.ShareResolverConfig{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	// 递归解析（Resolve 现支持子目录文件夹遍历）
	meta, err := r.Resolve(ctx, shareURL)
	if err != nil {
		cancel()
		log.Fatalf("resolve %s: %v", shareURL, err)
	}
	fmt.Printf("== share %s : %d files (递归) ==\n", meta.ShareID, len(meta.Files))
	shown := 0
	for i, f := range meta.Files {
		isVideo := isVideo(f.Name)
		if kindFilter == "video" && !isVideo {
			continue
		}
		if maxFiles > 0 && shown >= maxFiles {
			break
		}
		shown++
		path := f.Name
		fmt.Printf("[%d] %s\n", i, strings.TrimPrefix(path, "/"))
		fmt.Printf("    kind=%s size=%s\n", f.Kind, humanSize(f.Size))
		if f.DirectLink != "" {
			fmt.Printf("    url=%s\n", f.DirectLink)
		} else {
			fmt.Printf("    url=(folder, no direct link)\n")
		}
	}
	if shown == 0 {
		fmt.Println("（无匹配文件）")
	}
	cancel()
}

func isVideo(name string) bool {
	e := strings.ToLower(name)
	for _, ext := range []string{".mp4", ".mkv", ".avi", ".mov", ".flv", ".wmv", ".ts", ".m4v", ".webm"} {
		if strings.HasSuffix(e, ext) {
			return true
		}
	}
	return false
}

func humanSize(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
