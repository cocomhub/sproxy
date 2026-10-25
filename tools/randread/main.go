// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command randread 是外部卷**纯随机读**冷热性能基准工具（2026-10-25）。
//
// 场景：验证 blocklet 窗口缓存对随机访问（拖进度条/跳读）的命中效果——
// 冷：窗口未缓存 ⇒ 每随机片段触发窗口下载（受网盘限速）；热：窗口已缓存 ⇒ 内存命中。
//
// 用法：
//
//	randread -s https://host:port -f <url-encoded-rel> -n <file-size> -m <samples>
//
// 每次随机 4MiB 片段（与 /download/chunk 语义一致），输出每样本耗时/速率 + 聚合。
// 纯标准库；独立连接池（禁共享 DefaultTransport）。
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"time"

	"log/slog"

	"github.com/cocomhub/sproxy/pkg/client"
)

func main() {
	server := flag.String("s", "", "server base URL")
	fn := flag.String("f", "", "filename (URL-encoded rel, no volume param)")
	size := flag.Int64("n", 0, "file size in bytes")
	samples := flag.Int("m", 16, "number of random 4MiB samples")
	verbose := flag.Bool("v", false, "per-sample output")
	// 独立连接池（禁共享 DefaultTransport）；自签 TLS 用 --insecure（跳过校验）。
	insecure := flag.Bool("insecure", false, "跳过 TLS 证书校验（自签）")
	ak := flag.String("ak", "", "SproxySig AccessKey")
	sk := flag.String("sk", "", "SproxySig AccessKeySecret")
	id := flag.String("id", "", "SproxySig skeyID")
	flag.Parse()
	if *server == "" || *fn == "" || *size <= 0 {
		fmt.Fprintln(os.Stderr, "usage: randread -s <base> -f <encoded> -n <size> [-m samples] [-v]")
		os.Exit(2)
	}
	tr := &http.Transport{}
	if *insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 自签调试
	}
	base := &http.Client{Transport: tr}
	defer base.CloseIdleConnections()

	// 可选 SproxySig 签名：用 pkg/client 的 doRequest（带签名 + 认证头装配）。
	var hc *http.Client
	var fc *client.FileClient
	if *ak != "" && *sk != "" {
		fc = client.NewFileClient(*server,
			client.WithAccessKey(*ak, *sk),
			client.WithAccessKeyID(*id),
			client.WithHTTPClient(base),
			client.WithLogger(discardLogger()),
		)
		hc = base
	} else {
		hc = base
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	const win = 4 << 20
	// 随机覆盖文件不同区域（确保不集中在前段缓存区）。
	offs := make([]int64, *samples)
	maxOff := *size - win
	if maxOff <= 0 {
		maxOff = *size
	}
	for i := range offs {
		offs[i] = rng.Int63n(maxOff)
	}

	var total time.Duration
	var bytes int64
	var okN int
	ctx := context.Background()
	for i, off := range offs {
		url := fmt.Sprintf("%s/download/chunk?filename=%s&offset=%d&length=%d", *server, *fn, off, win)
		t0 := time.Now()
		var resp *http.Response
		var err error
		if fc != nil {
			// 带 SproxySig 签名（空 body GET；client 拼接 serverURL，传相对路径）。
			rel := fmt.Sprintf("/download/chunk?filename=%s&offset=%d&length=%d", *fn, off, win)
			resp, err = fc.RequestRaw(ctx, http.MethodGet, rel, nil, nil)
		} else {
			resp, err = hc.Get(url)
		}
		data, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		dur := time.Since(t0)
		if err != nil {
			fmt.Printf("[%2d] off=%9dMB err=%v\n", i, off>>20, err)
			continue
		}
		if rerr != nil || resp.StatusCode != http.StatusOK {
			fmt.Printf("[%2d] off=%9dMB status=%d err=%v\n", i, off>>20, resp.StatusCode, rerr)
			continue
		}
		if cs := resp.Header.Get("X-Chunk-Checksum"); cs != "" {
			h := sha256.Sum256(data)
			if got := hex.EncodeToString(h[:]); got != cs {
				fmt.Printf("[%2d] off=%9dMB CHECKSUM FAIL\n", i, off>>20)
				continue
			}
		}
		okN++
		total += dur
		bytes += int64(len(data))
		if *verbose {
			fmt.Printf("[%2d] off=%9dMB dur=%6dms  %6.1fMB/s\n", i, off>>20,
				dur.Milliseconds(), float64(len(data))/dur.Seconds()/1048576)
		}
	}
	if okN > 0 {
		fmt.Printf("\n随机读 %d 样本: 平均 %v/样本, 聚合 %.1f MB/s\n",
			okN, (total / time.Duration(okN)).Round(time.Millisecond),
			float64(bytes)/total.Seconds()/1048576)
	} else {
		fmt.Println("无成功样本")
	}
}

// discardLogger 返回丢弃日志（工具安静运行）。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
