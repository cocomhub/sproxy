// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// hybrid-e2e 真实链路验证：HybridDownloader（分享直链前段 + 账号流量后段）
// vs 账号完整下载（网盘直链全段），md5 对比文件一致性。
package main

import (
	"context"
	"crypto/md5" //nolint:gosec // 测试工具：文件一致性对比用 md5（非安全用途）
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

var (
	shareURL = flag.String("share", "", "分享 URL（默认读 PIKPAK_SHARE_URL 环境变量；真实数据禁止入库）")
	outDir   = flag.String("out", "build/hybrid-e2e", "输出目录")
	chunkMB  = flag.Int64("chunk", 64, "分片大小 MB")
)

func main() {
	flag.Parse()
	if *shareURL == "" {
		*shareURL = os.Getenv("PIKPAK_SHARE_URL")
	}
	if *shareURL == "" {
		log.Fatal("分享 URL 必填：--share 或 PIKPAK_SHARE_URL（真实数据禁止入库，运行期传入）")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background() //nolint:gocritic // 测试工具：长时下载不设 ctx 超时（外部 timeout 管控）

	// 1. resolver（匿名）+ api（账号）
	resolver := pikpak.NewShareResolver(pikpak.ShareResolverConfig{})
	api := pikpak.NewAPI(pikpak.APIConfig{}, nil)

	// 2. hybrid 下载
	metrics := &pikpak.HybridMetrics{}
	hybrid, err := pikpak.NewHybridDownloader(pikpak.HybridConfig{
		Resolver:    resolver,
		API:         api,
		ChunkSize:   *chunkMB << 20,
		ShareRatio:  0.5,
		Concurrency: 4,
		AutoDelete:  true,
		Metrics:     metrics,
	})
	if err != nil {
		log.Fatalf("hybrid: %v", err)
	}
	hybridPath := filepath.Join(*outDir, "hybrid.mp4")
	log.Println("=== A. hybrid 下载（分享直链前段 + 账号流量后段）===")
	start := time.Now()
	res, err := hybrid.Download(ctx, *shareURL, hybridPath, func(downloaded, total int64) {
		if downloaded%(256<<20) == 0 {
			log.Printf("hybrid progress: %d MB", downloaded>>20)
		}
	})
	if err != nil {
		log.Fatalf("hybrid download: %v", err)
	}
	log.Printf("hybrid done: size=%d checksum=%s elapsed=%s", res.Size, res.Checksum[:16], time.Since(start))
	log.Printf("metrics: share_saved=%d share_fail=%d downgrade=%d acct_fail=%d",
		metrics.ShareBytesSaved.Load()>>20, metrics.ShareSegmentFailed.Load(),
		metrics.DowngradeTotal.Load(), metrics.AccountSegmentFailed.Load())

	// 3. 账号完整下载（网盘直链全段，不经 hybrid）
	log.Println("=== B. 账号完整下载（转存→FETCH 直链全段 Range）===")
	fullPath := filepath.Join(*outDir, "full.mp4")
	start = time.Now()
	if err := accountFullDownload(ctx, api, *shareURL, fullPath); err != nil {
		log.Fatalf("full download: %v", err)
	}
	log.Printf("full done: elapsed=%s", time.Since(start))

	// 4. md5 对比
	h1, s1 := fileMD5(hybridPath)
	h2, s2 := fileMD5(fullPath)
	log.Printf("hybrid: md5=%s size=%d", h1, s1)
	log.Printf("full:   md5=%s size=%d", h2, s2)
	if h1 == h2 && s1 == s2 {
		log.Println("=== PASS: hybrid 与账号完整下载文件一致 ===")
	} else {
		log.Fatalf("=== FAIL: md5 不一致（hybrid=%s full=%s）===", h1, h2)
	}
}

// accountFullDownload 账号完整下载：转存 → FETCH 直链 → Range 全段。
func accountFullDownload(ctx context.Context, api *pikpak.API, shareURL, destPath string) error {
	shareID, err := pikpak.ParseShareID(shareURL)
	if err != nil {
		return err
	}
	// 转存
	files, err := api.ListShareRecursive(ctx, shareID)
	if err != nil {
		return err
	}
	target := pikpak.PickLargestVideo(files)
	if target == nil {
		return fmt.Errorf("no video in share")
	}
	fileID, _, err := api.RestoreShare(ctx, shareID, []string{target.ID}, "")
	if err != nil {
		return err
	}
	// 定位转存文件
	driveFile, err := api.FindByID(ctx, fileID)
	if err != nil {
		// 回退：按名字+大小找
		driveFile, err = api.FindInDrive(ctx, target.Name, target.Size)
		if err != nil {
			return err
		}
	}
	// FETCH 直链
	link, err := api.DownloadLink(ctx, driveFile.ID)
	if err != nil {
		return err
	}
	// 全段 Range 下载（自研，复用 http.Client）
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return downloadRange(ctx, link, 0, target.Size, f)
}

// downloadRange Range 下载（0..size）。
func downloadRange(ctx context.Context, link string, start, size int64, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, size-1))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// fileMD5 计算文件 md5 + 大小。
func fileMD5(path string) (string, int64) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // 测试工具：一致性对比
	n, _ := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n
}
