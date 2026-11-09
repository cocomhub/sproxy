// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	bdlib "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
	"github.com/qjfoidnh/BaiduPCS-Go/requester"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/downloader"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/transfer"
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
	// **失败即清理**（P1 修复，2026-10-21）：此前只在成功路径 rename/删断点，任何失败
	// （含下面 ctx 取消、探测失败、写盘错误）都会把与文件等大的 `.part` **永久留在本地**
	// （用户卷缺省落到持久目录，大文件即 GB 级泄漏，且全仓无清扫器）。
	ok := false
	defer func() {
		_ = writer.Close()
		if !ok {
			_ = os.Remove(finalTmp)
		}
	}()

	// 4. 构造 Downloader：Range 并行 + 断点文件。
	cfg := newDownloadConfig(resumePath)

	der := downloader.NewDownloader(dlink, writer, cfg)
	// 每实例独立连接池（禁共享 DefaultTransport）+ **网盘客户端 UA**：下载器 worker 走
	// `client.Req(...)`（库的 `Req` 会把 `HTTPClient.UserAgent` 写进请求头）⇒ 设它即可让
	// 并行 Range 请求带上 `netdisk;P2SP;...`（否则 CDN 403 sign error；小文件单请求不触发 Range
	// 所以此前"只有大文件读失败"）。库自身 pan 下载流程的等效配置是 PanUA（默认 = NetdiskUA）。
	der.SetClient(newDownloadHTTPClient())
	der.SetDURLCheckFunc(func(cli *requester.HTTPClient, durl string) (int64, *http.Response, error) {
		// 用 Range 探测文件大小（与上游 BaiduPCSURLCheckFunc 同构）。
		return baiduDURLCheck(ctx, cli, durl)
	})

	// **取消必须传导**（P1 修复，2026-10-21）：此前 ctx 只包探测请求，Execute 无 ctx、
	// Cancel 从未调用 ⇒ 上层取消/超时/停机后仍把**整个文件**下完（goroutine 泄漏、带宽与
	// 本地盘被占，而调用方已放弃）——与上传侧（multiupload 的 `<-ctx.Done(); Cancel()`）同构。
	cancelWatch := make(chan struct{})
	defer close(cancelWatch)
	go func() {
		select {
		case <-ctx.Done():
			der.Cancel()
		case <-cancelWatch:
		}
	}()

	if err := der.Execute(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("baidupcs: download %q 已取消: %w", remotePath, cerr)
		}
		return fmt.Errorf("baidupcs: download %q: %w", remotePath, err)
	}
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("baidupcs: download %q 已取消: %w", remotePath, cerr)
	}
	_ = writer.Sync()

	// 5. 完成：rename part → 最终路径；删除断点文件。
	if err := os.Rename(finalTmp, localPath); err != nil {
		return fmt.Errorf("baidupcs: finalize %q: %w", localPath, err)
	}
	ok = true
	_ = os.Remove(resumePath)
	return nil
}

// maxDownloadBlockSize 单次 Range 请求的**上限**。百度 CDN 对非会员账号拒绝过大的 Range
// （实测：1/2/4MiB ⇒ 206；8/15MiB ⇒ 403 `31326 user is not authorized, hitcode:104`），
// 且库自己的 BlockSizeList 上限也是 4MiB（`requester/downloader.BlockSizeList`，见
// `downloader.SelectBlockSizeAndInitRangeGen`）。
//
// **必须显式切到块模式**：库默认 `RangeGenMode_Default` 的自适应生成器会按 `总大小/并发` 切块
// （120MiB + 并发 8 ⇒ ~15MiB 块）⇒ 触发 CDN 403；此时 `cfg.BlockSize` 是**无效**的
// （它只在 `RangeGenMode_BlockSize` 下生效）——这正是「大文件读回 403、小文件正常」的根因。
const maxDownloadBlockSize = 4 << 20 // 4MiB

// newDownloadConfig 构造下载器配置：块模式 + 4MiB 块（≤ CDN/库上限）+ 断点文件。
func newDownloadConfig(resumePath string) *downloader.Config {
	cfg := downloader.NewConfig()
	cfg.MaxParallel = 8
	cfg.Mode = transfer.RangeGenMode_BlockSize
	cfg.BlockSize = maxDownloadBlockSize
	cfg.InstanceStateStorageFormat = downloader.InstanceStateStorageFormatJSON
	cfg.InstanceStatePath = resumePath
	return cfg
}

// newDownloadHTTPClient 构造下载器用的 HTTP 客户端：**每实例独立连接池**（禁共享
// DefaultTransport）+ **网盘客户端 UA**。
//
// 为什么必须设 UA：下载器 worker 走 `client.Req(...)`，库的 `Req` 会把 `HTTPClient.UserAgent`
// 写入请求头；而百度 CDN 对 **Range** 请求要求 `netdisk;P2SP;...`（缺/错 ⇒ 403 sign error 31362，
// 2026-10-22 实测）。不设 ⇒ 继承 `NewHTTPClient()` 的**浏览器 UA** ⇒ 大文件（并行 Range）读取 403、
// 小文件（单请求）正常。库自身 pan 下载流程的等效配置是 `PanUA`（默认 = `baidupcs.NetdiskUA`）。
func newDownloadHTTPClient() *requester.HTTPClient {
	cli := requester.NewHTTPClient()
	cli.SetUserAgent(bdlib.NetdiskUA)
	return cli
}

// baiduDURLCheck 用 Range 请求探测下载直链的**文件总大小**。
//
// P1 修复（2026-10-21）：此前直接取 `resp.ContentLength` / `Content-Length` 作为总大小，
// 而 dlink 对 Range 请求回 **206**，其 Content-Length 是**区间长度**（本函数请求 0-1023 ⇒ 1024）。
// 后果：库兜底路径（CLI 二进制缺失或 CLI 失败回落）把 1 KiB 当文件总大小 ⇒ ① 并行下载器拿
// `Content-Range` 的真实 total 与它比对 ⇒ `Content-Range total length is unexpected` ⇒ 任何
// >1 KiB 文件的读取**恒失败**；② 若中间层不回 Content-Range，则只写 1 KiB 就判成功 ⇒
// **静默截断**（数据损坏）。
//
// 探测优先级（x-bs-file-size 与上游 pcsupload/pcsdownload 同源）：
// `x-bs-file-size`（网盘显式给出的总大小）→ `Content-Range: bytes a-b/total` 的 total →
// 最后才 `Content-Length`（仅在 200 全量响应下才是总大小）。
func baiduDURLCheck(ctx context.Context, cli *requester.HTTPClient, durl string) (int64, *http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, durl, nil)
	if err != nil {
		return 0, nil, err
	}
	// **必须携带网盘客户端 UA**（与 syncfs_range.go 同因，2026-10-22 实测）：百度 CDN 对 Range
	// 请求要求 `netdisk;P2SP;...`，缺/错 ⇒ 403 `sign error (31362)`。注意 `cli.Do(req)` 是**裸**
	// http.Client.Do ⇒ 不应用 `HTTPClient.UserAgent`（那只在 `Req()` 里生效）⇒ 此处必须显式设。
	req.Header.Set("User-Agent", bdlib.NetdiskUA)
	// 探测区间与上游对齐（32KiB）：返回的 body 与首个 Range 块一致，避免探测浪费带宽。
	req.Header.Set("Range", "bytes=0-32767")
	resp, err := cli.Do(req)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return 0, nil, err
	}
	// 非 2xx（既不 200 也不 206）⇒ 把**响应体**带进错误：百度侧的 error_code/error_msg
	// （如 31362 sign error / 31066 file does not exist / 风控）是定性的唯一依据，
	// 此前只报 "403 Forbidden" 无法区分「UA/dlink 问题」与「可见性/权限问题」（实测踩过）。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
		return 0, nil, fmt.Errorf("baidupcs: dlink 探测 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return probeTotalSize(resp), resp, nil
}

// probeTotalSize 按优先级从探测响应解析**文件总大小**（解析不出时返回 0，由调用方判错）。
func probeTotalSize(resp *http.Response) int64 {
	if resp == nil {
		return 0
	}
	if v := resp.Header.Get("x-bs-file-size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	if n, ok := totalFromContentRange(resp.Header.Get("Content-Range")); ok {
		return n
	}
	// 206 的 Content-Length 是区间长度，**不能**当总大小（只有 200 才是全量）。
	if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
		return resp.ContentLength
	}
	if resp.StatusCode == http.StatusOK {
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// totalFromContentRange 解析 `bytes 0-32767/12345` 的总量（`*` 或缺失 ⇒ false）。
func totalFromContentRange(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	slash := strings.LastIndexByte(v, '/')
	if slash < 0 || slash == len(v)-1 {
		return 0, false
	}
	tail := strings.TrimSpace(v[slash+1:])
	if tail == "*" {
		return 0, false
	}
	n, err := strconv.ParseInt(tail, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
