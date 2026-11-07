// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
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
	defer cancel()
	resolver := pikpak.NewShareResolver(pikpak.ShareResolverConfig{})
	api := pikpak.NewAPI(pikpak.APIConfig{}, nil)
	metrics := &pikpak.HybridMetrics{}
	hd, _ := pikpak.NewHybridDownloader(pikpak.HybridConfig{
		Resolver: resolver, API: api, ChunkSize: 16 << 20, ShareRatio: 0.5,
		Concurrency: 4, AutoDelete: false, Metrics: metrics,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	log.Println("=== resolve 先行 ===")
	meta, err := resolver.Resolve(ctx, *shareURL)
	log.Printf("resolve: files=%d err=%v", len(meta.Files), err) //nolint:gosec // G706 诊断工具日志（操作者自己的分享数据）
	if err == nil && len(meta.Files) > 0 {
		log.Printf("target: %s size=%d", meta.Files[0].Name, meta.Files[0].Size) //nolint:gosec // G706 诊断工具日志
	}
	log.Println("=== Download（阶段日志在 hybrid.go）===")
	_, err = hd.Download(ctx, *shareURL, "build/hybrid-e2e/diag_share.mp4", nil)
	log.Printf("download err=%v share_saved=%d share_fail=%d acct_fail=%d",
		err, metrics.ShareBytesSaved.Load(), metrics.ShareSegmentFailed.Load(), metrics.AccountSegmentFailed.Load())
}
