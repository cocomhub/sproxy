// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/qjfoidnh/BaiduPCS-Go/requester"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/downloader"
)

// downloadViaDownloader 用上游 Downloader（Range 并行 + 断点续传）下载网盘文件到本地。
//
// 断点：InstanceState 持久化到 <Layout.Tmp>/<key>.download（JSON 格式），
// 中断后重新调用时从文件恢复 RangeList，只下载未完成区间。
// 完成时删除断点文件。中间状态只依赖本地文件系统（用户硬约束）。
func downloadViaDownloader(ctx context.Context, pcs *Client, remotePath, localPath string, layout *Layout) error {
	if pcs == nil {
		return fmt.Errorf("baidupcs: download without client")
	}

	// 1. 获取下载直链（LocateDownload）。
	urlInfo, pcsErr := pcs.PCS().LocateDownload(remotePath)
	if pcsErr != nil {
		return fmt.Errorf("baidupcs: locate download %q: %w", remotePath, mapPCSError(pcsErr))
	}
	dlink := ""
	if len(urlInfo.URLs) > 0 {
		dlink = urlInfo.URLs[0].URL
	}
	if dlink == "" {
		return fmt.Errorf("baidupcs: no download url for %q", remotePath)
	}

	// 2. 断点文件（Layout.Tmp 下）。
	key := layout.SanitizeKey(remotePath)
	resumePath := filepath.Join(layout.Tmp, key+".download")

	// 3. 目标 writer（临时文件 → 完成后 rename 到最终路径，避免半截文件）。
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("baidupcs: mkdir %s: %w", dir, err)
	}
	finalTmp := localPath + ".part"
	writer, err := os.OpenFile(finalTmp, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return fmt.Errorf("baidupcs: open part file %s: %w", finalTmp, err)
	}
	defer writer.Close()

	// 4. 构造 Downloader：Range 并行 + 断点文件。
	cfg := downloader.NewConfig()
	cfg.MaxParallel = 4
	cfg.BlockSize = 4 * 1024 * 1024
	cfg.InstanceStateStorageFormat = downloader.InstanceStateStorageFormatJSON
	cfg.InstanceStatePath = resumePath

	der := downloader.NewDownloader(dlink, writer, cfg)
	der.SetClient(requester.NewHTTPClient()) // 每实例独立连接池（禁共享 DefaultTransport）
	der.SetDURLCheckFunc(func(cli *requester.HTTPClient, durl string) (int64, *http.Response, error) {
		// 用 Range 探测文件大小（与上游 BaiduPCSURLCheckFunc 同构）。
		return baiduDURLCheck(ctx, cli, durl)
	})

	if err := der.Execute(); err != nil {
		return fmt.Errorf("baidupcs: download %q: %w", remotePath, err)
	}
	_ = writer.Sync()

	// 5. 完成：rename part → 最终路径；删除断点文件。
	if err := os.Rename(finalTmp, localPath); err != nil {
		return fmt.Errorf("baidupcs: finalize %q: %w", localPath, err)
	}
	_ = os.Remove(resumePath)
	return nil
}

// baiduDURLCheck 用 Range 请求探测下载直链的文件大小（返回 contentLength）。
// 仅用于 Downloader 的初始探测；正文传输由 Downloader 内部并行 Range 完成。
func baiduDURLCheck(ctx context.Context, cli *requester.HTTPClient, durl string) (int64, *http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, durl, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Range", "bytes=0-1023")
	resp, err := cli.Do(req)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return 0, nil, err
	}
	contentLength := resp.ContentLength
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, perr := strconv.ParseInt(cl, 10, 64); perr == nil {
			contentLength = n
		}
	}
	return contentLength, resp, nil
}
