// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	shareURL := "https://mypikpak.com/s/VP2z2zzw5oC9h71F8p8jGVq1o2"
	resolver := pikpak.NewShareResolver(pikpak.ShareResolverConfig{})
	api := pikpak.NewAPI(pikpak.APIConfig{}, nil)
	metrics := &pikpak.HybridMetrics{}
	hd, _ := pikpak.NewHybridDownloader(pikpak.HybridConfig{
		Resolver: resolver, API: api, ChunkSize: 16 << 20, ShareRatio: 0.5,
		Concurrency: 4, AutoDelete: false, Metrics: metrics,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	log.Println("=== resolve 先行 ===")
	meta, err := resolver.Resolve(ctx, shareURL)
	log.Printf("resolve: files=%d err=%v", len(meta.Files), err)
	if err == nil && len(meta.Files) > 0 {
		log.Printf("target: %s size=%d", meta.Files[0].Name, meta.Files[0].Size)
	}
	log.Println("=== Download（阶段日志在 hybrid.go）===")
	_, err = hd.Download(ctx, shareURL, "build/hybrid-e2e/diag_share.mp4", nil)
	log.Printf("download err=%v share_saved=%d share_fail=%d acct_fail=%d",
		err, metrics.ShareBytesSaved.Load(), metrics.ShareSegmentFailed.Load(), metrics.AccountSegmentFailed.Load())
}
