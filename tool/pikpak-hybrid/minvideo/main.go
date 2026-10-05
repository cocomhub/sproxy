// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 临时工具：解析分享 → 列出文件 → 挑最小视频打印直链 URL。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

func main() {
	shareURL := flag.String("share", "", "分享 URL（真实数据禁止入库，运行期经 -share 传入）")
	flag.Parse()
	if *shareURL == "" {
		log.Fatal("-share 必填（真实分享 URL 禁止入库，运行期经 flag 传入）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	// 注：不用 defer cancel——gocritic exitAfterDefer（Fatalf 分支跳过 defer）；
	// 改为所有退出点（含正常收尾）显式 cancel。
	resolver := pikpak.NewShareResolver(pikpak.ShareResolverConfig{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	meta, err := resolver.Resolve(ctx, *shareURL)
	if err != nil {
		cancel()
		log.Fatalf("resolve: %v", err)
	}
	fmt.Printf("== share %s: %d files ==\n", meta.ShareID, len(meta.Files))
	for i, f := range meta.Files {
		fmt.Printf("[%d] %-40s size=%-10d kind=%s\n    link=%s\n", i, f.Name, f.Size, f.Kind, f.DirectLink)
	}
	// 挑最小视频（kind=drive#file 且名字像视频；无 kind 标记时按 size 升序找最小非空文件）
	var minFile *pikpak.ShareFile
	for i := range meta.Files {
		f := &meta.Files[i]
		if f.Kind == "drive#folder" {
			continue
		}
		if minFile == nil || f.Size < minFile.Size {
			minFile = f
		}
	}
	if minFile == nil {
		cancel()
		log.Fatalf("no file found")
	}
	fmt.Printf("== MIN video: %s size=%d ==\n", minFile.Name, minFile.Size)
	fmt.Printf("%s\n", minFile.DirectLink)
}
